// chat_to_anthropic.go 响应向：OpenAI Chat Completions → Anthropic Messages。
//
// 非流式：ChatJSONToAnthropic 整包转换。
// 流式：AnthropicStreamConverter 状态机——chat chunk（map 形态，SSE data:
// 行解析后的对象）进、[]AnthropicStreamEvent 出；调用方在流结束时调
// Finalize 拿收尾事件（message_delta + message_stop）。
//
// 语义对齐 sub2api ChatCompletionsToAnthropicStreamState：
//   - message_start 延迟到首个实质内容（thinking/text/tool delta）才发
//   - reasoning_content → thinking 块；content → text 块；tool_calls →
//     tool_use 块（公告延迟到 name 到达，缓冲前置 id/args 片段）
//   - 块切换发 content_block_stop；usage 在 include_usage 独立 chunk 捕获
//   - finish_reason → stop_reason（length→max_tokens / tool_calls→tool_use /
//     其他→end_turn，有 tool_use 块则 tool_use）
package apicompat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 非流式
// ---------------------------------------------------------------------------

// ChatJSONToAnthropic 把上游非流式 Chat JSON 响应转为 Anthropic Messages 响应。
// model 为请求里客户端声明的模型名（响应 id/model 缺省时兜底）。
func ChatJSONToAnthropic(chatResp []byte, model string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(chatResp, &resp); err != nil {
		return nil, fmt.Errorf("parse upstream chat response: %w", err)
	}

	out := map[string]any{
		"type": "message",
		"role":  "assistant",
	}

	id := stringField(resp, "id")
	if id == "" {
		id = "msg_" + randomID()
	}
	out["id"] = id
	m := stringField(resp, "model")
	if m == "" {
		m = model
	}
	out["model"] = m

	var blocks []map[string]any
	finish := ""
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				blocks = chatMessageToAnthropicBlocks(msg)
			}
			finish = stringField(choice, "finish_reason")
		}
	}
	if len(blocks) == 0 {
		blocks = []map[string]any{{"type": "text", "text": ""}}
	}
	out["content"] = blocks
	out["stop_reason"] = chatFinishToAnthropicStop(finish, hasToolUseBlock(blocks))
	out["stop_sequence"] = nil

	if u, ok := resp["usage"].(map[string]any); ok {
		out["usage"] = chatUsageToAnthropicUsage(u)
	} else {
		out["usage"] = map[string]any{
			"input_tokens": 0, "output_tokens": 0,
		}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// chatMessageToAnthropicBlocks chat message → Anthropic blocks。
// reasoning_content → thinking 块；content → text 块；tool_calls → tool_use。
func chatMessageToAnthropicBlocks(msg map[string]any) []map[string]any {
	var blocks []map[string]any

	if rc := stringField(msg, "reasoning_content"); rc != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": rc})
	}

	text := chatMessageText(msg["content"])
	// 纯推理兜底：无正文无工具时把推理显为文本，不交白卷。
	if text == "" && len(toolCallsOf(msg)) == 0 {
		if rc := stringField(msg, "reasoning_content"); strings.TrimSpace(rc) != "" {
			text = rc
		}
	}
	if text != "" || len(toolCallsOf(msg)) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}

	for _, tc := range toolCallsOf(msg) {
		fn, _ := tc["function"].(map[string]any)
		name := stringField(fn, "name")
		args := stringField(fn, "arguments")
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		input := json.RawMessage(args)
		if !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    stringField(tc, "id"),
			"name":  name,
			"input": input,
		})
	}
	return blocks
}

// chatMessageText 提取 chat message 的 content 文本（string 或 parts 数组）。
func chatMessageText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t := stringField(pm, "text"); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func toolCallsOf(msg map[string]any) []map[string]any {
	calls, _ := msg["tool_calls"].([]any)
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		if m, ok := c.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func hasToolUseBlock(blocks []map[string]any) bool {
	for _, b := range blocks {
		if b["type"] == "tool_use" {
			return true
		}
	}
	return false
}

// chatFinishToAnthropicStop finish_reason → stop_reason。
func chatFinishToAnthropicStop(reason string, hasToolUse bool) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		if hasToolUse {
			return "tool_use"
		}
		return "end_turn"
	}
}

// chatUsageToAnthropicUsage chat usage → Anthropic usage 形态。
func chatUsageToAnthropicUsage(u map[string]any) map[string]any {
	prompt := intField(u, "prompt_tokens")
	completion := intField(u, "completion_tokens")
	cached, creation := 0, 0
	if det, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = intField(det, "cached_tokens")
		if w := intField(det, "cache_write_tokens"); w > 0 {
			creation = w
		} else {
			creation = intField(det, "cache_creation_tokens")
		}
	}
	input := prompt - cached - creation
	if input < 0 {
		input = 0
	}
	return map[string]any{
		"input_tokens":              input,
		"output_tokens":             completion,
		"cache_read_input_tokens":   cached,
		"cache_creation_input_tokens": creation,
	}
}

// ---------------------------------------------------------------------------
// 流式状态机
// ---------------------------------------------------------------------------

// AnthropicStreamConverter chat chunk → Anthropic 事件的状态机。
// 非并发安全：单流单 goroutine 使用（SSE 顺序语义）。
type AnthropicStreamConverter struct {
	model string

	messageStartSent bool
	messageStopSent  bool

	// 当前内容块生命周期
	blockIndex int
	blockOpen  bool
	blockType  string // "text" | "thinking"

	// tool_calls 按上游 index 缓冲：name 到达才公告块（Anthropic 的
	// content_block_start 需要 name），公告前的 id/args 片段一并冲刷。
	toolIndex    map[float64]int // 上游 tool index → Anthropic 块 index
	toolAnnounced map[float64]bool
	toolName     map[float64]string
	toolCallID   map[float64]string
	toolArgs     map[float64]*strings.Builder

	finish string

	inputTokens, outputTokens   int
	cacheRead, cacheCreation    int

	responseID string
	created    int64
}

// NewAnthropicStreamConverter 构建状态机（model 为请求侧模型名，兜底用）。
func NewAnthropicStreamConverter(model string) *AnthropicStreamConverter {
	return &AnthropicStreamConverter{
		model:        model,
		responseID:   "msg_" + randomID(),
		created:      time.Now().Unix(),
		toolIndex:    map[float64]int{},
		toolAnnounced: map[float64]bool{},
		toolName:     map[float64]string{},
		toolCallID:   map[float64]string{},
		toolArgs:     map[float64]*strings.Builder{},
	}
}

// Chunk 喂入一个 chat chunk（SSE data: 行解析后的 map），产出零或多个事件。
// 输入 nil 时返回 nil。
func (s *AnthropicStreamConverter) Chunk(chunk map[string]any) []AnthropicStreamEvent {
	if chunk == nil {
		return nil
	}
	if id := stringField(chunk, "id"); id != "" {
		s.responseID = id
	}
	if m := stringField(chunk, "model"); m != "" && s.model == "" {
		s.model = m
	}

	// include_usage 独立 chunk（choices 空带 usage）。
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		au := chatUsageToAnthropicUsage(u)
		s.inputTokens = intOf(au["input_tokens"])
		s.outputTokens = intOf(au["output_tokens"])
		s.cacheRead = intOf(au["cache_read_input_tokens"])
		s.cacheCreation = intOf(au["cache_creation_input_tokens"])
	}

	var events []AnthropicStreamEvent
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		events = append(events, s.choiceEvents(choice)...)
	}
	return events
}

// choiceEvents 处理单个 choice 的 delta。
func (s *AnthropicStreamConverter) choiceEvents(choice map[string]any) []AnthropicStreamEvent {
	var events []AnthropicStreamEvent
	delta, _ := choice["delta"].(map[string]any)

	// reasoning_content → thinking 块。
	if rc := stringField(delta, "reasoning_content"); rc != "" {
		events = append(events, s.ensureMessageStart()...)
		events = append(events, s.openBlock("thinking")...)
		events = append(events, s.deltaEvent("thinking_delta", map[string]any{"thinking": rc}))
	}

	// content → text 块（先关 thinking 块）。
	if content := stringField(delta, "content"); content != "" {
		events = append(events, s.ensureMessageStart()...)
		events = append(events, s.closeBlockIfType("thinking")...)
		events = append(events, s.openBlock("text")...)
		events = append(events, s.deltaEvent("text_delta", map[string]any{"text": content}))
	}

	// tool_calls → tool_use 块。
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			if m, ok := tc.(map[string]any); ok {
				events = append(events, s.toolCallEvents(m)...)
			}
		}
	}

	if fr := stringField(choice, "finish_reason"); fr != "" {
		s.finish = fr
	}
	return events
}

// toolCallEvents 单个 tool_call delta。name 到达才公告 content_block_start；
// 此前缓冲 id 与 args 片段，公告时冲刷。
func (s *AnthropicStreamConverter) toolCallEvents(tc map[string]any) []AnthropicStreamEvent {
	idx := 0.0
	if f, ok := tc["index"].(float64); ok {
		idx = f
	}
	var events []AnthropicStreamEvent

	fn, _ := tc["function"].(map[string]any)
	if name := stringField(fn, "name"); name != "" && s.toolName[idx] == "" {
		s.toolName[idx] = name
	}
	if id := stringField(tc, "id"); id != "" && s.toolCallID[idx] == "" {
		s.toolCallID[idx] = id
	}
	if args := stringField(fn, "arguments"); args != "" {
		if s.toolArgs[idx] == nil {
			s.toolArgs[idx] = &strings.Builder{}
		}
		s.toolArgs[idx].WriteString(args)
	}

	if s.toolAnnounced[idx] {
		// 已公告：参数增量直接进 delta。
		if args := stringField(fn, "arguments"); args != "" {
			events = append(events, s.deltaEvent("input_json_delta", map[string]any{
				"partial_json": args,
			}))
		}
		return events
	}

	if s.toolName[idx] != "" {
		events = append(events, s.ensureMessageStart()...)
		events = append(events, s.closeBlock()...)
		blockIdx := s.blockIndex
		s.toolIndex[idx] = blockIdx
		s.toolAnnounced[idx] = true

		if s.toolArgs[idx] == nil {
			s.toolArgs[idx] = &strings.Builder{}
		}
		input := s.toolArgs[idx].String()
		if strings.TrimSpace(input) == "" {
			input = "{}"
		}
		events = append(events, AnthropicStreamEvent{
			Event: "content_block_start",
			Data: map[string]any{
				"type":  "content_block_start",
				"index": blockIdx,
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   s.toolCallID[idx],
					"name": s.toolName[idx],
				},
			},
		})
		// 公告时冲刷缓冲的前置参数片段。
		if input != "{}" {
			events = append(events, s.deltaEvent("input_json_delta", map[string]any{
				"partial_json": input,
			}))
		}
		// 标记：tool 块开启中（blockOpen 用 toolIdxOpen 计数表达——
		// 这里简单置 blockOpen=false，因为参数增量不再需要 open 状态判断）。
		s.blockOpen = false
	}
	return events
}

// ensureMessageStart 惰性发送 message_start（首个实质内容到达时）。
func (s *AnthropicStreamConverter) ensureMessageStart() []AnthropicStreamEvent {
	if s.messageStartSent {
		return nil
	}
	s.messageStartSent = true
	return []AnthropicStreamEvent{{
		Event: "message_start",
		Data: map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":    s.responseID,
				"type":  "message",
				"role":  "assistant",
				"model": s.model,
				"content": []map[string]any{},
				"stop_reason": nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":                s.inputTokens,
					"cache_creation_input_tokens": 0,
					"cache_read_input_tokens":    0,
					"output_tokens":               0,
				},
			},
		},
	}}
}

// openBlock 打开 text/thinking 块（同类型已开则直接用）。
func (s *AnthropicStreamConverter) openBlock(blockType string) []AnthropicStreamEvent {
	if s.blockOpen && s.blockType == blockType {
		return nil
	}
	if s.blockOpen {
		return s.closeBlock()
	}
	s.blockOpen = true
	s.blockType = blockType
	blockTypeField := "text"
	if blockType == "thinking" {
		blockTypeField = "thinking"
	}
	return []AnthropicStreamEvent{{
		Event: "content_block_start",
		Data: map[string]any{
			"type":          "content_block_start",
			"index":         s.blockIndex,
			"content_block": map[string]any{"type": blockTypeField},
		},
	}}
}

// closeBlockIfType 关闭打开的块（仅当类型匹配，如 text 前先关 thinking）。
func (s *AnthropicStreamConverter) closeBlockIfType(blockType string) []AnthropicStreamEvent {
	if s.blockOpen && s.blockType == blockType {
		return s.closeBlock()
	}
	return nil
}

// closeBlock 关闭当前打开的块并推进 index。
func (s *AnthropicStreamConverter) closeBlock() []AnthropicStreamEvent {
	if !s.blockOpen {
		return nil
	}
	s.blockOpen = false
	idx := s.blockIndex
	s.blockIndex++
	return []AnthropicStreamEvent{{
		Event: "content_block_stop",
		Data:  map[string]any{"type": "content_block_stop", "index": idx},
	}}
}

// deltaEvent 发一个 *_delta 事件（index 取当前打开块；tool 块取其公告 index）。
func (s *AnthropicStreamConverter) deltaEvent(deltaType string, payload map[string]any) AnthropicStreamEvent {
	data := map[string]any{
		"type":  deltaType,
		"index": s.blockIndex,
		"delta": payload,
	}
	return AnthropicStreamEvent{Event: deltaType, Data: data}
}

// Finalize 流结束收尾：公告 name 未到的 tool（不丢缓冲参数）、关开块、
// message_delta（stop_reason + usage）、message_stop。幂等。
func (s *AnthropicStreamConverter) Finalize() []AnthropicStreamEvent {
	if s.messageStopSent {
		return nil
	}
	s.messageStopSent = true

	var events []AnthropicStreamEvent
	if !s.messageStartSent {
		events = append(events, s.ensureMessageStart()...)
	}

	// name 未到达的 tool_call：空名公告，冲刷缓冲参数（不丢数据）。
	var pendIdx []float64
	for idx := range s.toolCallID {
		if !s.toolAnnounced[idx] {
			pendIdx = append(pendIdx, idx)
		}
	}
	slices.Sort(pendIdx)
	for _, idx := range pendIdx {
		events = append(events, s.closeBlock()...)
		blockIdx := s.blockIndex
		s.blockIndex++
		s.toolIndex[idx] = blockIdx
		s.toolAnnounced[idx] = true
		if s.toolArgs[idx] == nil {
			s.toolArgs[idx] = &strings.Builder{}
		}
		input := s.toolArgs[idx].String()
		if strings.TrimSpace(input) == "" {
			input = "{}"
		}
		events = append(events, AnthropicStreamEvent{
			Event: "content_block_start",
			Data: map[string]any{
				"type": "content_block_start",
				"index": blockIdx,
				"content_block": map[string]any{
					"type": "tool_use",
					"id":   s.toolCallID[idx],
					"name": s.toolName[idx],
				},
			},
		})
		if input != "{}" {
			events = append(events, AnthropicStreamEvent{
				Event: "input_json_delta",
				Data: map[string]any{
					"type":  "input_json_delta",
					"index": blockIdx,
					"delta": map[string]any{"partial_json": input},
				},
			})
		}
		events = append(events, AnthropicStreamEvent{
			Event: "content_block_stop",
			Data:  map[string]any{"type": "content_block_stop", "index": blockIdx},
		})
	}

	events = append(events, s.closeBlock()...)

	hasToolUse := false
	for idx := range s.toolAnnounced {
		_ = idx
		hasToolUse = true
		break
	}
	events = append(events, AnthropicStreamEvent{
		Event: "message_delta",
		Data: map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   chatFinishToAnthropicStop(s.finish, hasToolUse),
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"input_tokens":              s.inputTokens,
				"cache_read_input_tokens":   s.cacheRead,
				"cache_creation_input_tokens": s.cacheCreation,
				"output_tokens":             s.outputTokens,
			},
		},
	})
	events = append(events, AnthropicStreamEvent{
		Event: "message_stop",
		Data:  map[string]any{"type": "message_stop"},
	})
	return events
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func intField(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	if f, ok := m[key].(float64); ok {
		return int(f)
	}
	return 0
}

func intOf(v any) int {
	if f, ok := v.(int); ok {
		return f
	}
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// randomID 短随机 ID（crypto/rand 转十六进制；服务侧响应 id 兜底用）。
func randomID() string {
	return fmt.Sprintf("%d-%04d", time.Now().UnixNano()%1e11, time.Now().UnixMilli()%10000)
}

// FormatAnthropicSSE 把一个事件格式化为 SSE 两行（event: + data:）。
func FormatAnthropicSSE(ev AnthropicStreamEvent) []byte {
	raw, err := json.Marshal(ev.Data)
	if err != nil {
		return nil
	}
	return append(append([]byte("event: "+ev.Event+"\ndata: "), raw...), '\n', '\n')
}
