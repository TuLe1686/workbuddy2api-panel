// chat_to_responses.go 响应向：OpenAI Chat Completions → OpenAI Responses。
//
// 非流式：ChatJSONToResponses 整包转换。
// 流式：ResponsesStreamConverter 状态机——简化事件集（官方 SDK 兼容的最小
// 完整序列）：response.created → output_item.added → content_part.added →
// output_text.delta（逐 content delta）→ output_item.done → response.completed。
// 工具参数**不发增量事件**，聚合在 output_item.done 的 function_call 里
// 全量给出（第一版减负决策，SDK 拼接语义兼容）。
// 事件行格式 "data: {...}\n\n"（Responses SSE 无 event: 行，类型在 type 字段）。
package apicompat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ChatJSONToResponses 把上游非流式 Chat JSON 响应转为 Responses 响应。
func ChatJSONToResponses(chatResp []byte, model string) ([]byte, error) {
	var resp map[string]any
	if err := json.Unmarshal(chatResp, &resp); err != nil {
		return nil, fmt.Errorf("parse upstream chat response: %w", err)
	}

	id := stringField(resp, "id")
	if id == "" {
		id = "resp_" + randomID()
	}
	m := stringField(resp, "model")
	if m == "" {
		m = model
	}

	var output []map[string]any
	finish := ""
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				output = chatMessageToResponsesOutput(msg)
			}
			finish = stringField(choice, "finish_reason")
		}
	}
	if len(output) == 0 {
		output = []map[string]any{emptyResponsesMessageItem()}
	}

	usage := map[string]any{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
	}
	if u, ok := resp["usage"].(map[string]any); ok {
		usage = chatUsageToResponsesUsage(u)
	}

	out := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      m,
		"output":     output,
		"usage":      usage,
		// 空流安全：finish=length 语义上仍是 completed（chat 上游截断信息
		// Responses 用 incomplete_details 表达，Codex 不依赖此字段决策）。
	}
	if finish == "length" {
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// chatMessageToResponsesOutput chat message → Responses output items。
func chatMessageToResponsesOutput(msg map[string]any) []map[string]any {
	var output []map[string]any

	text := chatMessageText(msg["content"])
	if rc := stringField(msg, "reasoning_content"); text == "" && strings.TrimSpace(rc) != "" && len(toolCallsOf(msg)) == 0 {
		// 纯推理兜底：显为文本，不交白卷。
		text = rc
	}
	if text != "" {
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "output_text", "text": text},
			},
		})
	}

	for _, tc := range toolCallsOf(msg) {
		fn, _ := tc["function"].(map[string]any)
		args := stringField(fn, "arguments")
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"call_id":   stringField(tc, "id"),
			"name":      stringField(fn, "name"),
			"arguments": args,
		})
	}

	if len(output) == 0 {
		output = append(output, emptyResponsesMessageItem())
	}
	return output
}

func emptyResponsesMessageItem() map[string]any {
	return map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{
			{"type": "output_text", "text": ""},
		},
	}
}

// chatUsageToResponsesUsage chat usage → Responses usage 形态。
func chatUsageToResponsesUsage(u map[string]any) map[string]any {
	prompt := intField(u, "prompt_tokens")
	completion := intField(u, "completion_tokens")
	total := intField(u, "total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	out := map[string]any{
		"input_tokens":  prompt,
		"output_tokens": completion,
		"total_tokens":  total,
	}
	if det, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if cached := intField(det, "cached_tokens"); cached > 0 {
			out["input_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 流式状态机
// ---------------------------------------------------------------------------

// ResponsesStreamConverter chat chunk → Responses 事件的状态机。
type ResponsesStreamConverter struct {
	model string

	createdSent bool
	doneSent    bool

	responseID string
	created    int64

	// message item 生命周期
	itemAddedSent bool
	partAddedSent bool
	text          strings.Builder
	hasText       bool

	// tool_calls 聚合（index → {id,name,args}），done 时全量发。
	tools map[float64]*responsesToolAgg

	finish string

	inputTokens, outputTokens, totalTokens int
	cachedTokens                           int
}

type responsesToolAgg struct {
	id   string
	name string
	args strings.Builder
}

// NewResponsesStreamConverter 构建状态机。
func NewResponsesStreamConverter(model string) *ResponsesStreamConverter {
	return &ResponsesStreamConverter{
		model:      model,
		responseID: "resp_" + randomID(),
		created:    time.Now().Unix(),
		tools:      map[float64]*responsesToolAgg{},
	}
}

// Chunk 喂入一个 chat chunk，产出零或多个事件（map 形态，写出行由调用方序列化）。
func (s *ResponsesStreamConverter) Chunk(chunk map[string]any) []map[string]any {
	if chunk == nil {
		return nil
	}
	if id := stringField(chunk, "id"); id != "" {
		s.responseID = id
	}
	if m := stringField(chunk, "model"); m != "" && s.model == "" {
		s.model = m
	}

	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		s.inputTokens = intField(u, "prompt_tokens")
		s.outputTokens = intField(u, "completion_tokens")
		s.totalTokens = intField(u, "total_tokens")
		if det, ok := u["prompt_tokens_details"].(map[string]any); ok {
			s.cachedTokens = intField(det, "cached_tokens")
		}
	}

	var events []map[string]any
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

func (s *ResponsesStreamConverter) choiceEvents(choice map[string]any) []map[string]any {
	var events []map[string]any
	delta, _ := choice["delta"].(map[string]any)

	if content := stringField(delta, "content"); content != "" {
		events = append(events, s.ensureCreated()...)
		events = append(events, s.ensureMessageItemOpen()...)
		s.text.WriteString(content)
		s.hasText = true
		events = append(events, map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       messageItemID,
			"output_index":  0,
			"content_index": 0,
			"delta":         content,
		})
	}

	if tcs, ok := delta["tool_calls"].([]any); ok {
		events = append(events, s.ensureCreated()...)
		for _, tc := range tcs {
			if m, ok := tc.(map[string]any); ok {
				s.accumulateTool(m)
			}
		}
	}

	if fr := stringField(choice, "finish_reason"); fr != "" {
		s.finish = fr
	}
	return events
}

// messageItemID message output item 的固定 id（简化：单文本 item）。
const messageItemID = "msg_item_0"

// accumulateTool 聚合 tool_call 片段（不发增量事件，done 全量发）。
func (s *ResponsesStreamConverter) accumulateTool(tc map[string]any) {
	idx := 0.0
	if f, ok := tc["index"].(float64); ok {
		idx = f
	}
	agg := s.tools[idx]
	if agg == nil {
		agg = &responsesToolAgg{}
		s.tools[idx] = agg
	}
	if id := stringField(tc, "id"); id != "" && agg.id == "" {
		agg.id = id
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if name := stringField(fn, "name"); name != "" && agg.name == "" {
			agg.name = name
		}
		if args := stringField(fn, "arguments"); args != "" {
			agg.args.WriteString(args)
		}
	}
}

// ensureCreated 惰性发 response.created。
func (s *ResponsesStreamConverter) ensureCreated() []map[string]any {
	if s.createdSent {
		return nil
	}
	s.createdSent = true
	return []map[string]any{{
		"type": "response.created",
		"response": map[string]any{
			"id":         s.responseID,
			"object":     "response",
			"created_at": s.created,
			"status":     "in_progress",
			"model":      s.model,
		},
	}}
}

// ensureMessageItemOpen 惰性发 message item 的 added + part.added。
func (s *ResponsesStreamConverter) ensureMessageItemOpen() []map[string]any {
	if s.itemAddedSent {
		return nil
	}
	s.itemAddedSent = true
	return []map[string]any{
		{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item": map[string]any{
				"id":   messageItemID,
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": ""},
				},
			},
		},
		{
			"type":          "response.content_part.added",
			"item_id":       messageItemID,
			"output_index":  0,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": ""},
		},
	}
}

// Finalize 流结束收尾：message item done（若有文本）、tool item done（全量
// 参数）、response.completed（带 usage）。幂等。
func (s *ResponsesStreamConverter) Finalize() []map[string]any {
	if s.doneSent {
		return nil
	}
	s.doneSent = true

	var events []map[string]any
	events = append(events, s.ensureCreated()...)

	// 文本 item done
	if s.hasText || len(s.tools) == 0 {
		if !s.itemAddedSent {
			events = append(events, s.ensureMessageItemOpen()...)
		}
		events = append(events, map[string]any{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item": map[string]any{
				"id":   messageItemID,
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": s.text.String()},
				},
			},
		})
	}

	// tool items done（按 index 升序）
	for _, idx := range sortedToolIdx(s.tools) {
		agg := s.tools[idx]
		args := agg.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			args = "{}"
		}
		events = append(events, map[string]any{
			"type":         "response.output_item.added",
			"output_index": len(events),
			"item": map[string]any{
				"id":   agg.id,
				"type": "function_call",
				"name": agg.name,
			},
		})
		events = append(events, map[string]any{
			"type":         "response.output_item.done",
			"output_index": len(events),
			"item": map[string]any{
				"id":        agg.id,
				"type":      "function_call",
				"call_id":   agg.id,
				"name":      agg.name,
				"arguments": args,
			},
		})
	}

	// response.completed
	usage := map[string]any{
		"input_tokens":  s.inputTokens,
		"output_tokens": s.outputTokens,
		"total_tokens":  s.totalTokens,
	}
	if s.totalTokens == 0 {
		usage["total_tokens"] = s.inputTokens + s.outputTokens
	}
	if s.cachedTokens > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": s.cachedTokens}
	}
	response := map[string]any{
		"id":         s.responseID,
		"object":     "response",
		"created_at": s.created,
		"status":     "completed",
		"model":      s.model,
		"usage":      usage,
	}
	if s.finish == "length" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	events = append(events, map[string]any{
		"type":     "response.completed",
		"response": response,
	})
	return events
}

func sortedToolIdx(tools map[float64]*responsesToolAgg) []float64 {
	out := make([]float64, 0, len(tools))
	for idx := range tools {
		out = append(out, idx)
	}
	slices.Sort(out)
	return out
}

// FormatResponsesSSE 把一个事件 map 序列化为 SSE 行。
func FormatResponsesSSE(ev map[string]any) []byte {
	raw, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), raw...), '\n', '\n')
}
