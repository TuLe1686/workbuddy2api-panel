// fallback.go 内容审核兜底渠道。
//
// 本池账号对一段内容连续被上游内容审核拒绝后（上限 MaxRotate），与其把 400
// 退回客户端，不如把同一段对话原样转发到一条审核更松的 OpenAI 兼容渠道。
// 兜底只在内容审核这条路上触发：限流、封号、余额耗尽仍走既有轮转，不外抛。
package server

import (
	"bytes"
	"context"
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
	// Timeout 单次兜底请求的总时长上限。<=0 时用 120s。
	Timeout time.Duration
	// HTTP 测试注入；nil 时用带超时的默认客户端。
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

// forward 把原始请求体转发到兜底渠道的 /chat/completions，只改 model 字段。
// 流式与非流式都原样透传：兜底渠道返回什么状态码，客户端就看到什么。
//
// 失败分两层。渠道本身返回的 HTTP 响应（含 4xx/5xx）算「转发成功」，原样写回，
// 返回 (状态码, nil)——客户端看到的是渠道的真实答复。只有连不上、超时才返回
// error，调用方据此退回本池的内容审核原文。
func (f ContentFallbackConfig) forward(ctx context.Context, w http.ResponseWriter, body []byte, requestModel, fallbackModel string, stream bool) (int, error) {
	out := rewriteModel(body, fallbackModel)
	url := strings.TrimRight(f.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(out))
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
		t := f.Timeout
		if t <= 0 {
			t = 120 * time.Second
		}
		c = &http.Client{Timeout: t}
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fallback upstream: %w", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// 头已发出，只能记日志；对调用方仍算转发完成（客户端已经在读了）。
		log.Printf("WARN: [server] content fallback copy: %v", err)
	}
	return resp.StatusCode, nil
}
