// Package apicompat 入站协议转换：Anthropic Messages / OpenAI Responses ↔
// OpenAI Chat Completions 双向转换（纯函数，不依赖 server/pool/upstream——
// server 侧经 convertingWriter 包装后 100% 复用既有 chatCompletions 链路）。
//
// 设计参照 sub2api 的 chatcompletions_anthropic_bridge / chatcompletions_responses_bridge
// （直接桥：入站协议 → Chat 单跳，不经过中间表示），但类型体系按本网关的
// 最小需求重写：结构体只覆盖必须感知的字段，请求侧未知字段一律丢弃
// （出站 body 由本包完全生成，不存在"透传未知字段"的通道）。
//
// 响应方向分两个入口：
//   - 非流式：ChatJSONToAnthropic / ChatJSONToResponses（整包转换）
//   - 流式：  anthropicStreamState / responsesStreamState 状态机（chunk 进、
//     协议事件出；按 SSE data: 行逐个 chunk 喂入，状态机内部保块生命周期）
package apicompat

import "encoding/json"

// ---------------------------------------------------------------------------
// Anthropic Messages 最小类型（请求侧）
// ---------------------------------------------------------------------------

// AnthropicRequest POST /v1/messages 请求体。只声明网关需要感知的字段；
// metadata/top_k/anthropic_beta 等未知字段丢弃。
type AnthropicRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system"`  // string 或 []AnthropicContentBlock
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`      // Anthropic 必填
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Tools         []AnthropicTool `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Thinking      *AnthropicThinking `json:"thinking"`
}

// AnthropicThinking 思考开关（Anthropic 形态 {type:"enabled",budget_tokens:N}）。
type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

// AnthropicMessage 一条消息。Content 是 string 或 []AnthropicContentBlock。
type AnthropicMessage struct {
	Role    string          `json:"role"` // "user" | "assistant"
	Content json.RawMessage `json:"content"`
}

// AnthropicTool 工具定义。Type "function"（input_schema 为 JSON Schema）；
// web_search_* 服务器工具在转换层丢弃。
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Type        string          `json:"type"` // "custom"（默认，即 function）
}

// AnthropicContentBlock 消息内容块（text/image/thinking/tool_use/tool_result/
// server_tool_use/web_search_tool_result/redacted_thinking）。
type AnthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// thinking 块
	Thinking   string `json:"thinking"`
	Signature  string `json:"signature"`
	RedactedData string `json:"data"` // redacted_thinking 的密文

	// tool_use 块
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result 块
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result 的内容（string 或块数组）
	IsError   bool            `json:"is_error"`

	// image 块
	Source *AnthropicImageSource `json:"source"`
}

// AnthropicImageSource 图片源（base64 形态；URL 形态本网关上游用不了，转占位）。
type AnthropicImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// ---------------------------------------------------------------------------
// Anthropic 流式事件（响应侧）
// ---------------------------------------------------------------------------

// AnthropicStreamEvent 一个 SSE 事件（event: 行 + data: 行）。
type AnthropicStreamEvent struct {
	Event string
	Data  map[string]any
}

// ---------------------------------------------------------------------------
// OpenAI Responses 最小类型（请求侧）
// ---------------------------------------------------------------------------

// ResponsesRequest POST /v1/responses 请求体。Input 是 string 或
// []item（message/function_call/function_call_output/reasoning/…），
// item 侧用 json.RawMessage 按需判 type。
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           json.RawMessage `json:"input"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Tools           []ResponsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Reasoning       *ResponsesReasoning `json:"reasoning"`
	Text            json.RawMessage `json:"text"` // {format:{...}, verbosity:...}
	ParallelToolCalls *bool         `json:"parallel_tool_calls"`
}

// ResponsesReasoning 思考参数 {effort:"low|medium|high", summary:...}。
type ResponsesReasoning struct {
	Effort  string          `json:"effort"`
	Summary json.RawMessage `json:"summary"` // 丢弃（上游无摘要通道）
}

// ResponsesTool 工具定义。Type "function" 才转换，其余（web_search/custom/
// local_shell/…）第一版丢弃。
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}
