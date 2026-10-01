package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// sseThinkingTool 上游响应（OpenAI 官方流形态：tool name 在首片）：thinking →
// text → tool_call → usage。网关 StreamHint 的 stripToolCallNames 按「name 恒
// 在首片」收敛，name 后置的分片会被剥 name——fixture 必须贴合真实上游形态。
const sseThinkingTool = "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想一下\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"答案是\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"credit\":0.5}}\n\n" +
	"data: [DONE]\n\n"

// chatSyncTool 非流式客户端请求的上游响应：SSE 形态（上游恒流式；网关对
// 非流式客户端请求用 Aggregate 聚合 SSE 成 JSON）。
const chatSyncTool = "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"思考\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"我来看下\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
	"data: [DONE]\n\n"

func postProto(h *Handler, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const anthropicReq = `{
  "model": "glm-5.2", "max_tokens": 1024, "stream": true,
  "system": "You are helpful.",
  "messages": [{"role":"user","content":"hi"}]
}`

// /v1/messages 流式全链路：fake 上游 → Anthropic 事件序列。
func TestAnthropicMessagesStreamEndToEnd(t *testing.T) {
	withChatLog(t)
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 200, sseThinkingTool, true
	})
	rec := postProto(h, "/v1/messages", "static-key", anthropicReq)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"\"type\":\"thinking\"",
		"event: thinking_delta",
		"event: text_delta",
		"event: content_block_start",
		"\"type\":\"tool_use\"",
		"\"name\":\"bash\"",
		"event: input_json_delta",
		"event: message_delta",
		"\"stop_reason\":\"tool_use\"",
		"\"output_tokens\":5",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("流缺 %q:\n%s", want, body)
		}
	}
	// message_stop 只出现一次（双发回归）
	if n := strings.Count(body, "event: message_stop"); n != 1 {
		t.Errorf("message_stop 出现 %d 次应 1 次", n)
	}
	// tool 参数完整拼接
	var args strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "partial_json") {
			var d struct {
				Delta struct {
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) == nil {
				args.WriteString(d.Delta.PartialJSON)
			}
		}
	}
	if args.String() != `{"cmd":"ls"}` {
		t.Errorf("tool 参数拼接 = %q", args.String())
	}
}

// /v1/messages 非流式。
func TestAnthropicMessagesNonStreamEndToEnd(t *testing.T) {
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 200, chatSyncTool, true
	})
	// stream:false：上游恒 SSE，网关 Aggregate 聚合后由 wrapper 转 Anthropic JSON
	req := `{"model":"glm-5.2","max_tokens":100,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	rec := postProto(h, "/v1/messages", "static-key", req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应非 JSON: %v\n%s", err, rec.Body)
	}
	if m["type"] != "message" {
		t.Errorf("type = %v", m["type"])
	}
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", m["stop_reason"])
	}
	blocks := m["content"].([]any)
	if len(blocks) != 3 || blocks[0].(map[string]any)["type"] != "thinking" || blocks[1].(map[string]any)["type"] != "text" || blocks[2].(map[string]any)["type"] != "tool_use" {
		t.Errorf("blocks: %v", blocks)
	}
}

// 上游 4xx：错误响应转 Anthropic 错误格式（经 chatCompletions 错误透传链路）。
func TestAnthropicMessagesErrorFormat(t *testing.T) {
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	rec := postProto(h, "/v1/messages", "static-key", anthropicReq)
	if rec.Code == 200 {
		t.Fatalf("应非 200: %s", rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("错误体非 JSON: %s", rec.Body)
	}
	if m["type"] != "error" {
		t.Errorf("错误应为 Anthropic 形态: %s", rec.Body)
	}
	e, _ := m["error"].(map[string]any)
	if e["message"] == "" {
		t.Errorf("error.message 应保留: %s", rec.Body)
	}
}

// key 限额 429：转 Anthropic rate_limit_error 格式；签发 key 归因同样生效。
func TestAnthropicMessagesKeyQuota429(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("cc用户", 1, 0, 0)
	keys.NoteUsage(k.ID, 100, true, 1.5, true) // 打满
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	rec := postProto(h, "/v1/messages", k.Secret, anthropicReq)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "error" || m["error"].(map[string]any)["type"] != "rate_limit_error" {
		t.Errorf("429 应为 rate_limit_error: %s", rec.Body)
	}
	// 台账：被拒请求计 1 次
	got := keys.List()
	if len(got) != 1 || got[0].Requests != 2 { // NoteUsage 预置 1 + 被拒 1
		t.Errorf("台账: %+v", got)
	}
}

// 多 key 鉴权对新端点同样生效（错误 key 401，Anthropic 错误形态）。
func TestAnthropicMessagesAuth(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("x", 0, 0, 0)
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	if code := postProto(h, "/v1/messages", "wrong", anthropicReq).Code; code != 401 {
		t.Errorf("错误 key code=%d want 401", code)
	}
	if code := postProto(h, "/v1/messages", k.Secret, anthropicReq).Code; code != 200 {
		t.Errorf("签发 key code=%d want 200", code)
	}
	if code := postProto(h, "/v1/messages", "static-key", anthropicReq).Code; code != 200 {
		t.Errorf("静态 key code=%d want 200", code)
	}
}

// 协议入口转换失败（document 块）→ 400 Anthropic invalid_request_error。
func TestAnthropicMessagesBadRequest(t *testing.T) {
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	bad := `{"model":"glm-5.2","max_tokens":10,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"x"}}]}]}`
	rec := postProto(h, "/v1/messages", "static-key", bad)
	if rec.Code != 400 {
		t.Fatalf("code=%d", rec.Code)
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["type"] != "error" || m["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Errorf("400 错误形态: %s", rec.Body)
	}
}

const responsesReq = `{
  "model": "glm-5.2", "stream": true, "store": false,
  "instructions": "You are a coding agent.",
  "reasoning": {"effort": "medium"},
  "input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
}`

// /v1/responses 流式全链路。
func TestResponsesStreamEndToEnd(t *testing.T) {
	withChatLog(t)
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 200, sseThinkingTool, true
	})
	rec := postProto(h, "/v1/responses", "static-key", responsesReq)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// JSON 键序不保证（map marshal 按字母序），断言用事件类型子串而非前缀。
	for _, want := range []string{
		"\"type\":\"response.created\"",
		"\"type\":\"response.output_item.added\"",
		"\"type\":\"response.content_part.added\"",
		"\"type\":\"response.output_text.delta\"",
		"\"type\":\"response.output_item.done\"",
		"\"type\":\"function_call\"",
		"\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"",
		"\"type\":\"response.completed\"",
		"\"input_tokens\":10",
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("流缺 %q:\n%s", want, body)
		}
	}
	// completed 只出现一次
	if n := strings.Count(body, "response.completed"); n != 1 {
		t.Errorf("response.completed 出现 %d 次", n)
	}
}

// /v1/responses 非流式。
func TestResponsesNonStreamEndToEnd(t *testing.T) {
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 200, chatSyncTool, true
	})
	req := `{"model":"glm-5.2","input":"hi","store":false}`
	rec := postProto(h, "/v1/responses", "static-key", req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("外壳: %v", m)
	}
	items := m["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output items = %d: %v", len(items), m["output"])
	}
}

// Responses 错误透传（OpenAI 形态）+ 429 key 限额。
func TestResponsesErrorAndQuota(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("codex用户", 1, 0, 0)
	keys.NoteUsage(k.ID, 10, true, 1.0, true)
	h := newKeyedHandler(t, "static-key", keys, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	rec := postProto(h, "/v1/responses", k.Secret, responsesReq)
	if rec.Code != 429 {
		t.Fatalf("code=%d", rec.Code)
	}
	// Responses 错误即 OpenAI 形态（{"error":{message,...}}）
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["error"]; !ok {
		t.Errorf("Responses 错误应保持 OpenAI 形态: %s", rec.Body)
	}
}

// 并发上限对新端点生效（阻塞上游 + 上限 1 → 第二请求 429）。
func TestProtocolConcurrencyLimit(t *testing.T) {
	keys := apikeys.New("")
	k, _ := keys.Create("并发cc", 0, 0, 1)

	block := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	up := &blockingUpstream{onArrive: func() (<-chan struct{}, int, string, bool) {
		once.Do(func() { close(started) })
		return block, 200, sseOK, true
	}}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up.client(t),
		APIKey:   "static-key",
		Keys:     keys,
	})
	done := make(chan int, 2)
	go func() {
		done <- postProto(h, "/v1/messages", k.Secret, anthropicReq).Code
	}()
	<-started
	rec2 := postProto(h, "/v1/messages", k.Secret, anthropicReq)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("第二个并发请求 code=%d want 429", rec2.Code)
	}
	close(block)
	if code := <-done; code != 200 {
		t.Errorf("第一个请求 code=%d want 200", code)
	}
}

// 工具往返：入站 tool_result（Anthropic）/ function_call_output（Responses）
// 经转换正确送达上游链路（消息级转换已由 apicompat 单测覆盖；此处验证
// 端到端 200 + 响应可转换）。
func TestProtocolToolResultReachesUpstream(t *testing.T) {
	h := newKeyedHandler(t, "static-key", nil, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	toolReq := `{
	  "model":"glm-5.2","max_tokens":100,"stream":false,
	  "tools":[{"name":"bash","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}],
	  "messages":[
	    {"role":"user","content":"跑个命令"},
	    {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"cmd":"ls"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file1\nfile2"}]}
	  ]
	}`
	rec := postProto(h, "/v1/messages", "static-key", toolReq)
	if rec.Code != 200 {
		t.Fatalf("工具往返 code=%d body=%s", rec.Code, rec.Body)
	}
}
