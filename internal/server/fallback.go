// fallback.go 内容审核兜底渠道。
//
// 本池账号对一段内容连续被上游内容审核拒绝后（上限 MaxRotate），与其把 400
// 退回客户端，不如把同一段对话原样转发到一条审核更松的 OpenAI 兼容渠道。
// 兜底只在内容审核这条路上触发：限流、封号、余额耗尽仍走既有轮转，不外抛。
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ContentFallbackConfig 一条兜底渠道。零值（BaseURL 空）= 未配置，行为与没有兜底完全一致。
type ContentFallbackConfig struct {
	// BaseURL OpenAI 兼容根，如 https://api.example.com/v1（末尾斜杠无所谓）。
	BaseURL string
	// APIKey 兜底渠道的密钥，出站放 Authorization: Bearer。
	APIKey string
	// ModelMap 请求模型名 → 兜底渠道模型名。键用客户端发来的原名（含 realm 前缀，
	// 如 "cn:glm-5.3"），未命中的模型不兜底，仍按 400 退回。
	ModelMap map[string]string
	// Timeout 空闲超时：等响应头、以及流中两次收到数据之间，最多等这么久。
	// 持续有数据就一直转发，不设总时长上限。<=0 时用 120s。
	Timeout time.Duration
	// HTTP 测试注入；nil 时用不设总超时的默认客户端（超时由空闲计时负责）。
	HTTP *http.Client
}

// enabled 是否配了可用的兜底（地址与密钥都在）。
func (f ContentFallbackConfig) enabled() bool {
	return strings.TrimSpace(f.BaseURL) != "" && strings.TrimSpace(f.APIKey) != ""
}

// modelFor 返回该请求模型对应的兜底模型名。未映射返回 ("", false)：
// 没配映射的模型不猜、不转发，避免把审核内容打到渠道不认识的模型上。
func (f ContentFallbackConfig) modelFor(requestModel string) (string, bool) {
	if !f.enabled() {
		return "", false
	}
	m, ok := f.ModelMap[requestModel]
	return m, ok && strings.TrimSpace(m) != ""
}

func (f ContentFallbackConfig) idleTimeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return 120 * time.Second
}

// errFallbackIdle 空闲计时到点：渠道这么久没有新数据。
var errFallbackIdle = errors.New("content fallback idle timeout")

// idleTimer 空闲超时计时器：每收到一段数据就重置；到点取消请求上下文。
type idleTimer struct {
	t      *time.Timer
	d      time.Duration
	fired  chan struct{}
	cancel context.CancelCauseFunc
}

func newIdleTimer(d time.Duration, cancel context.CancelCauseFunc) *idleTimer {
	it := &idleTimer{d: d, fired: make(chan struct{}), cancel: cancel}
	it.t = time.AfterFunc(d, func() {
		close(it.fired)
		cancel(errFallbackIdle)
	})
	return it
}

// reset 续期；已到点返回 false（请求已被取消，续期无意义）。
func (it *idleTimer) reset() bool {
	select {
	case <-it.fired:
		return false
	default:
	}
	return it.t.Reset(it.d)
}

func (it *idleTimer) stop() { it.t.Stop() }

// forward 把请求体转发到兜底渠道的 /chat/completions，只改 model 字段。
// 流式与非流式都原样透传：兜底渠道返回什么状态码，客户端就看到什么。
// 每写出一段数据就 Flush，客户端与主路径一样边生成边收到。
//
// 失败分两层。拿到响应头之前出错（连不上、等头超时）返回 error，客户端还没
// 收到任何字节，调用方据此退回本池的内容审核原文。拿到响应头之后，渠道的
// 状态码与正文原样写回，返回 (状态码, nil)；中途断流只能记日志——头已发出。
func (f ContentFallbackConfig) forward(ctx context.Context, w http.ResponseWriter, body []byte, fallbackModel string, stream bool) (int, error) {
	out := rewriteModel(body, fallbackModel)
	url := strings.TrimRight(f.BaseURL, "/") + "/chat/completions"

	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := newIdleTimer(f.idleTimeout(), cancel)
	defer idle.stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(out))
	if err != nil {
		return 0, fmt.Errorf("build fallback request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	c := f.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		if cause := context.Cause(reqCtx); errors.Is(cause, errFallbackIdle) {
			return 0, fmt.Errorf("fallback upstream: no response header within %s", f.idleTimeout())
		}
		return 0, fmt.Errorf("fallback upstream: %w", err)
	}
	defer resp.Body.Close()
	idle.reset()

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	if strings.HasPrefix(ct, "text/event-stream") {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.reset()
			if _, werr := w.Write(buf[:n]); werr != nil {
				log.Printf("WARN: [server] content fallback write to client: %v", werr)
				return resp.StatusCode, nil
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if rerr == io.EOF {
			return resp.StatusCode, nil
		}
		if rerr != nil {
			if errors.Is(context.Cause(reqCtx), errFallbackIdle) {
				log.Printf("WARN: [server] content fallback stream idle > %s, cut", f.idleTimeout())
			} else {
				log.Printf("WARN: [server] content fallback read: %v", rerr)
			}
			return resp.StatusCode, nil
		}
	}
}
