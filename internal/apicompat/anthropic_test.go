package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeChat 把 chat body 解回 map（测试断言用）。
func decodeChat(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("chat body 不是合法 JSON: %v\n%s", err, body)
	}
	return m
}

// chatMessages 取 messages 数组的 map 形态。
func chatMessages(t *testing.T, chat map[string]any) []map[string]any {
	t.Helper()
	raw, _ := chat["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

func findMsg(msgs []map[string]any, role string) map[string]any {
	for _, m := range msgs {
		if m["role"] == role {
			return m
		}
	}
	return nil
}

// 金样本：Claude Code 形态请求——system blocks + tools + 多轮 tool_use/
// tool_result 回放 + thinking 回放 + 图片。
func TestAnthropicToChatFullRoundTrip(t *testing.T) {
	body := []byte(`{
  "model": "glm-5.2",
  "max_tokens": 8096,
  "stream": true,
  "temperature": 0.7,
  "system": [
    {"type":"text","text":"You are a helpful assistant.","cache_control":{"type":"ephemeral"}},
    {"type":"text","text":"Follow the project conventions."}
  ],
  "tools": [
    {"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
    {"type":"web_search_20250305","name":"search"}
  ],
  "tool_choice": {"type":"auto"},
  "messages": [
    {"role":"user","content":"帮我读一下 README"},
    {"role":"assistant","content":[
      {"type":"thinking","thinking":"用户要读 README，调用工具。","signature":"sig-ignored"},
      {"type":"tool_use","id":"toolu_01","name":"read_file","input":{"path":"README.md"}}
    ]},
    {"role":"user","content":[
      {"type":"tool_result","tool_use_id":"toolu_01","content":"# Hello\nWorld","is_error":false}
    ]},
    {"role":"assistant","content":[
      {"type":"text","text":"README 的内容如下："}
    ]}
  ]
}`)
	chat, stream, err := AnthropicToChat(body)
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
	if mt, ok := m["max_tokens"].(float64); !ok || int(mt) != 8096 {
		t.Errorf("max_tokens = %v want 8096", m["max_tokens"])
	}
	if temp, ok := m["temperature"].(float64); !ok || temp != 0.7 {
		t.Errorf("temperature = %v", m["temperature"])
	}

	msgs := chatMessages(t, m)
	// system + user + assistant(tool) + tool + assistant(text) = 5
	if len(msgs) != 5 {
		t.Fatalf("messages 数量 = %d want 5:\n%s", len(msgs), chat)
	}

	sys := findMsg(msgs, "system")
	if sys == nil || sys["content"] != "You are a helpful assistant.\n\nFollow the project conventions." {
		t.Errorf("system 折叠不符: %v", sys)
	}

	// 第二条 user（"帮我读一下 README"）
	u1 := msgs[1]
	if u1["role"] != "user" || u1["content"] != "帮我读一下 README" {
		t.Errorf("首条 user 不符: %v", u1)
	}

	// assistant 带 tool_calls + reasoning_content
	assistant1 := msgs[2]
	tcs, _ := assistant1["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %d want 1: %v", len(tcs), assistant1)
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "read_file" || fn["arguments"] != `{"path":"README.md"}` {
		t.Errorf("tool_call 不符: %v", tc)
	}
	if assistant1["reasoning_content"] != "用户要读 README，调用工具。" {
		t.Errorf("thinking 应转 reasoning_content: %v", assistant1["reasoning_content"])
	}

	// tool_result → tool role
	toolMsg := findMsg(msgs, "tool")
	if toolMsg == nil {
		t.Fatal("tool 消息缺失")
	}
	if toolMsg["tool_call_id"] != "toolu_01" || toolMsg["content"] != "# Hello\nWorld" {
		t.Errorf("tool 消息不符: %v", toolMsg)
	}

	// tools 转换：web_search 丢弃，只剩 read_file
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %d want 1（web_search 应丢弃）: %v", len(tools), m["tools"])
	}
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v want auto", m["tool_choice"])
	}
}

// 图片块 → parts 数组；纯文本 → string（严格 chat 上游拒绝数组 content）。
func TestAnthropicToChatImageParts(t *testing.T) {
	body := []byte(`{
  "model": "glm-5.2", "max_tokens": 100,
  "messages": [
    {"role":"user","content":[
      {"type":"text","text":"这是什么？"},
      {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aGVsbG8="}}
    ]}
  ]
}`)
	chat, _, err := AnthropicToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs := chatMessages(t, decodeChat(t, chat))
	u := findMsg(msgs, "user")
	parts, _ := u["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("带图应走 parts 数组: %v", u["content"])
	}
	textPart := parts[0].(map[string]any)
	imgPart := parts[1].(map[string]any)
	if textPart["text"] != "这是什么？" {
		t.Errorf("text part: %v", textPart)
	}
	iu := imgPart["image_url"].(map[string]any)
	if iu["url"] != "data:image/jpeg;base64,aGVsbG8=" {
		t.Errorf("image data URI: %v", iu)
	}
}

// thinking 预算 → reasoning_effort 粗映射。
func TestAnthropicThinkingEffortMap(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{2000, "low"}, {8000, "medium"}, {32000, "high"},
	}
	for _, c := range cases {
		body := []byte(`{"model":"glm-5.2","max_tokens":10,"thinking":{"type":"enabled","budget_tokens":` +
			jsonInt(c.budget) + `},"messages":[{"role":"user","content":"hi"}]}`)
		chat, _, err := AnthropicToChat(body)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeChat(t, chat)["reasoning_effort"]; got != c.want {
			t.Errorf("budget %d → effort %v want %v", c.budget, got, c.want)
		}
	}
	// disabled → 不带
	body := []byte(`{"model":"glm-5.2","max_tokens":10,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	chat, _, err := AnthropicToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := decodeChat(t, chat)["reasoning_effort"]; has {
		t.Error("disabled thinking 不应带 reasoning_effort")
	}
}

// document 块显式 400。
func TestAnthropicRejectsDocumentBlock(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","max_tokens":10,"messages":[{"role":"user","content":[
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"..."}}]}]}`)
	_, _, err := AnthropicToChat(body)
	if err == nil || !strings.Contains(err.Error(), "document") {
		t.Errorf("document 块应 400: %v", err)
	}
}

// tool_choice {type:"tool",name}：仅已声明转发，未声明丢弃。
func TestAnthropicToolChoiceNamed(t *testing.T) {
	base := `{"model":"glm-5.2","max_tokens":10,
  "tools":[{"name":"bash","description":"run","input_schema":{"type":"object"}}],
  "messages":[{"role":"user","content":"hi"}],`
	chat, _, err := AnthropicToChat([]byte(base + `"tool_choice":{"type":"tool","name":"bash"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if tc := decodeChat(t, chat)["tool_choice"]; tc == nil {
		t.Error("已声明 tool_choice 应转发")
	}
	chat2, _, err := AnthropicToChat([]byte(base + `"tool_choice":{"type":"tool","name":"unknown_tool"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := decodeChat(t, chat2)["tool_choice"]; has {
		t.Error("未声明的 tool_choice 应丢弃")
	}
}

// 非流式响应转换：reasoning/tool_calls/finish/usage 全链。
func TestChatJSONToAnthropicNonStream(t *testing.T) {
	chat := []byte(`{
  "id":"chatcmpl-abc","model":"glm-5.2",
  "choices":[{"index":0,"finish_reason":"tool_calls","message":{
    "role":"assistant","content":null,
    "reasoning_content":"思考中……",
    "tool_calls":[{"id":"call_01","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]
  }}],
  "usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,
    "prompt_tokens_details":{"cached_tokens":20}}
}`)
	out, err := ChatJSONToAnthropic(chat, "glm-5.2")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Errorf("外壳不符: %v", m)
	}
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v want tool_use", m["stop_reason"])
	}
	blocks := m["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d want 2（thinking + tool_use）: %v", len(blocks), blocks)
	}
	if blocks[0].(map[string]any)["type"] != "thinking" {
		t.Errorf("首块应为 thinking: %v", blocks[0])
	}
	tu := blocks[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "bash" {
		t.Errorf("tool_use 块: %v", tu)
	}
	u := m["usage"].(map[string]any)
	if int(u["input_tokens"].(float64)) != 80 || int(u["output_tokens"].(float64)) != 50 {
		t.Errorf("usage 不符（cached 20 应扣）: %v", u)
	}
	if int(u["cache_read_input_tokens"].(float64)) != 20 {
		t.Errorf("cache_read: %v", u)
	}
}

// 流式状态机金样本：thinking → text → tool_call（name 后置）→ usage → finish。
func TestAnthropicStreamConverter(t *testing.T) {
	s := NewAnthropicStreamConverter("glm-5.2")

	var events []AnthropicStreamEvent
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
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"先想一想"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"答案是"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"42"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"bash","arguments":"th\":\"x\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":90,"completion_tokens":12,"total_tokens":102,"credit":0.5}}`,
	)
	events = append(events, s.Finalize()...)

	// 事件序列断言
	var seq []string
	for _, e := range events {
		seq = append(seq, e.Event)
	}
	wantSeq := []string{
		"message_start",
		"content_block_start", // thinking
		"thinking_delta",
		"content_block_stop", // 关 thinking 开 text
		"content_block_start",
		"text_delta", "text_delta",
		"content_block_stop", // 关 text
		"content_block_start", // tool 公告（name 到达）
		"input_json_delta",   // 冲刷全部缓冲片段（含公告 chunk 自带的增量）
		"message_delta",
		"message_stop",
	}
	if len(seq) != len(wantSeq) {
		t.Fatalf("事件序列不符:\n got %v\nwant %v", seq, wantSeq)
	}
	for i := range seq {
		if seq[i] != wantSeq[i] {
			t.Fatalf("事件[%d] = %s want %s（全序列 got %v）", i, seq[i], wantSeq[i], seq)
		}
	}

	// message_delta 的 stop_reason 与 usage
	var deltaEv map[string]any
	for _, e := range events {
		if e.Event == "message_delta" {
			deltaEv = e.Data
		}
	}
	if deltaEv["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason: %v", deltaEv)
	}
	du := deltaEv["usage"].(map[string]any)
	if intOf(du["output_tokens"]) != 12 {
		t.Errorf("usage.output_tokens = %v want 12", du)
	}

	// tool 公告事件的 name 与冲刷参数
	var toolStart map[string]any
	for _, e := range events {
		if e.Event == "content_block_start" {
			cb := e.Data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				toolStart = e.Data
			}
		}
	}
	if toolStart == nil {
		t.Fatal("tool 公告缺失")
	}
	cb := toolStart["content_block"].(map[string]any)
	if cb["name"] != "bash" || cb["id"] != "call_9" {
		t.Errorf("tool 公告: %v", cb)
	}
	// 两段参数都应到达（缓冲 + 增量）
	var jsonArgs strings.Builder
	for _, e := range events {
		if e.Event == "input_json_delta" {
			jsonArgs.WriteString(e.Data["delta"].(map[string]any)["partial_json"].(string))
		}
	}
	if jsonArgs.String() != `{"path":"x"}` {
		t.Errorf("tool 参数拼接 = %q", jsonArgs.String())
	}
}

// name 永不到达的 tool：finalize 空名公告不丢参数。
func TestAnthropicStreamConverterToolNameNeverArrives(t *testing.T) {
	s := NewAnthropicStreamConverter("m")
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{\"a\":1}"}}]}}]}`), &m)
	_ = s.Chunk(m)
	events := s.Finalize()
	found := false
	for _, e := range events {
		if e.Event == "input_json_delta" {
			if e.Data["delta"].(map[string]any)["partial_json"] == `{"a":1}` {
				found = true
			}
		}
	}
	if !found {
		t.Error("name 未到达的 tool 参数不应丢失")
	}
}

// 错误格式转换。
func TestOpenAIErrorToAnthropic(t *testing.T) {
	out := OpenAIErrorToAnthropic([]byte(`{"error":{"message":"credit quota exceeded: 10/10","type":"api_error","code":"key_quota_exceeded"}}`))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "error" {
		t.Fatalf("外层 type: %v", m)
	}
	e := m["error"].(map[string]any)
	if e["type"] != "rate_limit_error" || e["message"] == "" {
		t.Errorf("error 块: %v", e)
	}
	// 非 OpenAI 形态原样透传
	weird := []byte(`{"some":"upstream body"}`)
	if string(OpenAIErrorToAnthropic(weird)) != string(weird) {
		t.Error("非 OpenAI 形态应原样透传")
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
