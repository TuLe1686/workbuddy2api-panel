package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// 金样本：Codex 形态请求——instructions + items（message/function_call/
// function_call_output/reasoning）+ reasoning.effort + tools。
func TestResponsesToChatFullRoundTrip(t *testing.T) {
	body := []byte(`{
  "model": "glm-5.2",
  "instructions": "You are a coding agent.",
  "stream": true,
  "max_output_tokens": 8192,
  "reasoning": {"effort": "medium", "summary": "auto"},
  "store": false,
  "tools": [
    {"type":"function","name":"shell","description":"Run a shell command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
    {"type":"web_search"}
  ],
  "tool_choice": "auto",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"列一下当前目录"}]},
    {"type":"reasoning","summary":[{"type":"summary_text","text":"需要跑 ls"}]},
    {"type":"function_call","call_id":"call_01","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
    {"type":"function_call_output","call_id":"call_01","output":"file_a.go\nfile_b.go"},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"解释一下"}]}
  ]
}`)
	chat, stream, err := ResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Error("stream 应透传 true")
	}
	m := decodeChat(t, chat)

	if m["model"] != "glm-5.2" {
		t.Errorf("model = %v", m["model"])
	}
	if mt, ok := m["max_tokens"].(float64); !ok || int(mt) != 8192 {
		t.Errorf("max_tokens = %v want 8192", m["max_tokens"])
	}
	if m["reasoning_effort"] != "medium" {
		t.Errorf("reasoning_effort = %v", m["reasoning_effort"])
	}
	if _, has := m["store"]; has {
		t.Error("store 应丢弃")
	}

	msgs := chatMessages(t, m)
	// system + user + assistant(tool) + tool + user = 5
	if len(msgs) != 5 {
		t.Fatalf("messages = %d want 5:\n%s", len(msgs), chat)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "You are a coding agent." {
		t.Errorf("instructions 应转 system: %v", msgs[0])
	}
	// reasoning item 挂到相邻 assistant tool_calls 消息
	assistant := msgs[2]
	tcs, _ := assistant["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls: %v", assistant)
	}
	if assistant["reasoning_content"] != "需要跑 ls" {
		t.Errorf("reasoning item 应回放到 assistant: %v", assistant["reasoning_content"])
	}
	toolMsg := findMsg(msgs, "tool")
	if toolMsg["tool_call_id"] != "call_01" || toolMsg["content"] != "file_a.go\nfile_b.go" {
		t.Errorf("function_call_output → tool: %v", toolMsg)
	}

	// web_search 工具丢弃
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d want 1: %v", len(tools), m["tools"])
	}
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", m["tool_choice"])
	}
}

// input 纯字符串形态 + image part。
func TestResponsesToChatStringInput(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","input":"你好","store":false}`)
	chat, _, err := ResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs := chatMessages(t, decodeChat(t, chat))
	if len(msgs) != 1 || msgs[0]["role"] != "user" || msgs[0]["content"] != "你好" {
		t.Fatalf("string input: %v", msgs)
	}
}

func TestResponsesToChatImagePart(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","input":[
	  {"type":"message","role":"user","content":[
	    {"type":"input_text","text":"看图"},
	    {"type":"input_image","image_url":"data:image/png;base64,QQ=="}
	  ]}
	]}`)
	chat, _, err := ResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs := chatMessages(t, decodeChat(t, chat))
	parts, _ := msgs[0]["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("带图应 parts 数组: %v", msgs[0]["content"])
	}
	iu := parts[1].(map[string]any)["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,QQ==" {
		t.Errorf("image_url: %v", iu)
	}
}

// text.format → response_format。
func TestResponsesTextFormat(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","input":"hi","text":{"format":{"type":"json_object"},"verbosity":"low"}}`)
	chat, _, err := ResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := decodeChat(t, chat)["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_object" {
		t.Errorf("response_format = %v", rf)
	}
	// format type=text → 不带
	body2 := []byte(`{"model":"glm-5.2","input":"hi","text":{"format":{"type":"text"}}}`)
	chat2, _, err := ResponsesToChat(body2)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := decodeChat(t, chat2)["response_format"]; has {
		t.Error("format type=text 不应带 response_format")
	}
}

// 非流式响应转换。
func TestChatJSONToResponsesNonStream(t *testing.T) {
	chat := []byte(`{
  "id":"chatcmpl-x","model":"glm-5.2",
  "choices":[{"index":0,"finish_reason":"tool_calls","message":{
    "role":"assistant","content":"我来看下",
    "tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]
  }}],
  "usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":70,
    "prompt_tokens_details":{"cached_tokens":10}}
}`)
	out, err := ChatJSONToResponses(chat, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("外壳: %v", m)
	}
	items := m["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output items = %d want 2: %v", len(items), m["output"])
	}
	msgItem := items[0].(map[string]any)
	if msgItem["type"] != "message" {
		t.Errorf("首 item 应 message: %v", msgItem)
	}
	fc := items[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "shell" {
		t.Errorf("function_call item: %v", fc)
	}
	u := m["usage"].(map[string]any)
	if int(u["input_tokens"].(float64)) != 50 || int(u["total_tokens"].(float64)) != 70 {
		t.Errorf("usage: %v", u)
	}
	if int(u["input_tokens_details"].(map[string]any)["cached_tokens"].(float64)) != 10 {
		t.Errorf("cached: %v", u)
	}
}

// 流式状态机：created → item.added/part.added → text.delta ×2 → item.done →
// tool done（全量参数）→ completed（usage）。
func TestResponsesStreamConverter(t *testing.T) {
	s := NewResponsesStreamConverter("glm-5.2")

	var events []map[string]any
	feed := func(chunks ...string) {
		for _, c := range chunks {
			var m map[string]any
			if err := json.Unmarshal([]byte(c), &m); err != nil {
				t.Fatalf("bad chunk: %v", err)
			}
			events = append(events, s.Chunk(m)...)
		}
	}
	feed(
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"先"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"看看"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"shell","arguments":"{\"cmd\""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"ls\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":8,"total_tokens":38}}`,
	)
	events = append(events, s.Finalize()...)

	var seq []string
	for _, e := range events {
		seq = append(seq, e["type"].(string))
	}
	wantSeq := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.done",
		"response.output_item.added", // tool item
		"response.output_item.done",  // tool item（全量参数）
		"response.completed",
	}
	if len(seq) != len(wantSeq) {
		t.Fatalf("事件序列不符:\n got %v\nwant %v", seq, wantSeq)
	}
	for i := range seq {
		if seq[i] != wantSeq[i] {
			t.Fatalf("事件[%d] = %s want %s（got %v）", i, seq[i], wantSeq[i], seq)
		}
	}

	// 文本 item done 的 text 为全量拼接
	for _, e := range events {
		if e["type"] == "response.output_item.done" {
			item := e["item"].(map[string]any)
			if item["type"] == "message" {
				content := asAnySlice(item["content"])[0].(map[string]any)
				if content["text"] != "先看看" {
					t.Errorf("text 拼接 = %v", content["text"])
				}
			}
			if item["type"] == "function_call" {
				if item["arguments"] != `{"cmd":"ls"}` {
					t.Errorf("tool 参数 = %v", item["arguments"])
				}
				if item["name"] != "shell" || item["call_id"] != "call_2" {
					t.Errorf("tool item: %v", item)
				}
			}
		}
	}

	// completed 的 usage
	var completed map[string]any
	for _, e := range events {
		if e["type"] == "response.completed" {
			completed = e
		}
	}
	u := completed["response"].(map[string]any)["usage"].(map[string]any)
	if intOf(u["input_tokens"]) != 30 || intOf(u["total_tokens"]) != 38 {
		t.Errorf("completed usage: %v", u)
	}
}

// SSE 格式：data: 行 + \n\n。
func TestFormatResponsesSSE(t *testing.T) {
	out := FormatResponsesSSE(map[string]any{"type": "response.created"})
	if !strings.HasPrefix(string(out), "data: {") || !strings.HasSuffix(string(out), "\n\n") {
		t.Errorf("SSE 格式: %q", out)
	}
}

// Anthropic SSE 格式：event: 行 + data: 行。
func TestFormatAnthropicSSE(t *testing.T) {
	out := FormatAnthropicSSE(AnthropicStreamEvent{Event: "message_stop", Data: map[string]any{"type": "message_stop"}})
	if !strings.HasPrefix(string(out), "event: message_stop\ndata: ") || !strings.HasSuffix(string(out), "\n\n") {
		t.Errorf("SSE 格式: %q", out)
	}
}

// asAnySlice 兼容 []any 与 []map[string]any（未序列化的 map 字面量形态）。
func asAnySlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []map[string]any:
		out := make([]any, len(s))
		for i, m := range s {
			out[i] = m
		}
		return out
	}
	return nil
}
