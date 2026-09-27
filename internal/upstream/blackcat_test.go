package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// sseNightOK 夜猫子对话的最小成功 SSE（一帧 delta + DONE）。
const sseNightOK = "data: {\"id\":\"n1\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
	"data: [DONE]\n\n"

// 11140 分野（2026-09-28）：同一个 code+msg 双语义——内容审核 displayMsg 归
// ErrContentBlocked（不罚号），纯 request illegal 保持 ErrAccountFault（Disable）。
// 背景：夜猫子任务触发的审核拒绝被误判封号，一夜误禁 32 个健康账号。

func TestClassify11140ContentReviewIsContentBlocked(t *testing.T) {
	body := `{"code":11140,"msg":"request illegal","requestId":"ec4455","displayMsg":{"en":"Content failed safety review. Please revise it","zh":"内容未通过安全审核，请调整"}}`
	if got := Classify(403, body); got != ErrContentBlocked {
		t.Fatalf("带内容审核 displayMsg 的 11140 应归 ErrContentBlocked（不罚号），got %v", got)
	}
	// 中文 displayMsg 单独出现（无英文）也要命中。
	bodyZh := `{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过安全审核，请调整"}}`
	if got := Classify(403, bodyZh); got != ErrContentBlocked {
		t.Fatalf("中文 displayMsg 形态应归 ErrContentBlocked，got %v", got)
	}
	// 英文小写形态（ToLower 路径）。
	bodyEn := `{"code":11140,"msg":"request illegal","displayMsg":{"en":"content failed safety review. please revise it"}}`
	if got := Classify(403, bodyEn); got != ErrContentBlocked {
		t.Fatalf("英文小写 displayMsg 应归 ErrContentBlocked，got %v", got)
	}
}

func TestClassify11140PureIllegalStillAccountFault(t *testing.T) {
	// 纯 request illegal（无审核 displayMsg）= 账号级授权封禁，语义不变。
	body := `{"code":11140,"msg":"request illegal","requestId":"abc"}`
	if got := Classify(403, body); got != ErrAccountFault {
		t.Fatalf("纯 11140 应保持 ErrAccountFault（Disable 语义），got %v", got)
	}
	// 401 形态的账号风控同样保持。
	body401 := `{"code":11140,"msg":"request illegal"}`
	if got := Classify(401, body401); got != ErrAccountFault {
		t.Fatalf("401 纯 11140 应保持 ErrAccountFault，got %v", got)
	}
	// 14017 语义不受分野影响。
	if got := Classify(403, `{"code":14017,"msg":"trial not activated"}`); got != ErrAccountFault {
		t.Fatalf("14017 应保持 ErrAccountFault，got %v", got)
	}
}

// 文案池：确定性散列 + 同号同晚互不重复 + 跨账号散开。
func TestPickNightPromptDeterministicAndSpread(t *testing.T) {
	a, b := "uid-aaa", "uid-bbb"
	date := "2026-09-28"
	// 确定性：同输入恒同输出。
	if pickNightPrompt(a, date, 0) != pickNightPrompt(a, date, 0) {
		t.Fatal("同输入应确定性同输出")
	}
	// 同号同晚 3 次互不相同（need≤3；池 16 条，冲突概率极低但断言真实意图）。
	first := pickNightPrompt(a, date, 0)
	second := pickNightPrompt(a, date, 1)
	third := pickNightPrompt(a, date, 2)
	if first == second || second == third || first == third {
		t.Fatalf("同号同晚 3 次应取不同文案: %q %q %q", first, second, third)
	}
	// 全部来自池内。
	for _, nth := range []int{0, 1, 2} {
		found := false
		for _, p := range nightPrompts {
			if pickNightPrompt(a, date, nth) == p {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("文案必须来自 nightPrompts 池")
		}
	}
	// 跨账号散开：两个不同账号的第一次对话大概率不同（167 号场景下必然）。
	diff := 0
	for i := 0; i < 100; i++ {
		ua := a + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if pickNightPrompt(ua, date, 0) != pickNightPrompt(b, date, 0) {
			diff++
		}
	}
	if diff < 90 {
		t.Fatalf("跨账号散开不足（100 个相邻 uid 仅 %d 个与 b 不同）", diff)
	}
	// 池内无敏感固定模板（旧文案必须移除）。
	for _, p := range nightPrompts {
		if p == "1+1等于几？直接回答。" {
			t.Fatal("旧模板文案必须从池中移除")
		}
	}
	if len(nightPrompts) < 12 {
		t.Fatalf("文案池应有足够多样性（≥12），当前 %d", len(nightPrompts))
	}
}

// 夜猫子出站体（2026-09-28 治本回归）：RunNightChats 发出的 body 必须是
// 「system + 池内 user 文案 + 经 prepareBody 出站链」的真实客户端形态，
// 不再是旧的无 system 裸单条模板请求。用 rtFunc 捕获真实出站体断言。
func TestRunNightChatsSendsRealisticBody(t *testing.T) {
	var captured []byte
	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		buf, _ := io.ReadAll(r.Body)
		// 只捕获 chat 出站体（report 上报走 JSON 返回即可，别覆盖 captured）。
		if strings.Contains(r.URL.Path, "/chat/completions") {
			captured = buf
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseNightOK)),
			}, nil
		}
		return jsonResp(200, `{"code":0}`), nil
	})
	c := &Client{
		HTTP:       &http.Client{Transport: transport},
		ChatHTTP:   &http.Client{Transport: transport},
		ChatBaseCN: "https://fake.example",
	}
	a := &auth.Auth{UID: "uid-x", AccessToken: "at-x", ExpiresAt: 9999999999}
	if _, err := c.RunNightChats(a, 1); err != nil {
		t.Fatalf("RunNightChats: %v", err)
	}
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		PromptCacheKey any `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("出站体不是合法 JSON: %v\n%s", err, captured)
	}
	if got.Model != "glm-5.2" {
		t.Fatalf("model=%s want glm-5.2", got.Model)
	}
	if len(got.Messages) < 2 || got.Messages[0].Role != "system" {
		t.Fatalf("必须带 system 消息（真实客户端形态），got %+v", got.Messages)
	}
	user := got.Messages[len(got.Messages)-1]
	if user.Role != "user" {
		t.Fatalf("末条必须是 user，got %s", user.Role)
	}
	inPool := false
	for _, p := range nightPrompts {
		if user.Content == p {
			inPool = true
			break
		}
	}
	if !inPool {
		t.Fatalf("user 文案必须来自 nightPrompts 池，got %q", user.Content)
	}
	if user.Content == "1+1等于几？直接回答。" {
		t.Fatal("旧模板文案不得再出现")
	}
	if got.PromptCacheKey == nil {
		t.Fatal("出站体必须经 prepareBody（prompt_cache_key 已注入），裸 body 是审核异常形态")
	}
}
