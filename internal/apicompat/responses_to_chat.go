// responses_to_chat.go 请求向：OpenAI Responses → OpenAI Chat Completions。
//
// 语义对齐 sub2api responsesInputToChatMessages + buildChatMessagesFromItems：
//   - instructions → system message
//   - input（string 或 items[]）：message / function_call / function_call_output /
//     reasoning item 逐个转换；reasoning 的文本挂到相邻 assistant 消息的
//     reasoning_content（DeepSeek 思考回放语义，与 Messages 桥一致）
//   - reasoning.effort → reasoning_effort（走网关降级管线）
//   - text.format → response_format 透传
//   - store / previous_response_id / prompt_cache_key / include / summary /
//     verbosity 丢弃（网关无状态，Codex 全量回放不依赖服务端会话）
package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesToChat 把 /v1/responses 请求体转换为 Chat Completions 请求体。
// 返回 (chatBody, stream, error)。model 保留原样（realm 前缀由 server 剥）。
func ResponsesToChat(body []byte) ([]byte, bool, error) {
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, fmt.Errorf("parse responses request: %w", err)
	}
	if req.Model == "" {
		return nil, false, fmt.Errorf("model is required")
	}

	out := map[string]any{"model": req.Model}
	if req.Stream {
		out["stream"] = true
	}
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = maxTokensClamp(req.MaxOutputTokens)
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}

	messages, err := responsesInputToChat(req.Instructions, req.Input)
	if err != nil {
		return nil, false, err
	}
	out["messages"] = messages

	if tools, declared := responsesToolsToChat(req.Tools); len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := responsesToolChoiceToChat(req.ToolChoice, declared); ok {
			out["tool_choice"] = tc
		}
	}

	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}

	// text.format → response_format 透传（{"type":"text"} 无意义，跳过）。
	if rf, ok := responsesTextFormat(req.Text); ok {
		out["response_format"] = rf
	}

	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	chatBody, err := json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	return chatBody, req.Stream, nil
}

// responsesTextFormat 从 text 字段提取 response_format。
// text = {"format":{"type":"json_object"|...}, "verbosity":...}；format.type
// 为 "text" 时无操作（chat 缺省就是 text）。
func responsesTextFormat(textRaw json.RawMessage) (any, bool) {
	if len(textRaw) == 0 {
		return nil, false
	}
	var text struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(textRaw, &text); err != nil || len(text.Format) == 0 {
		return nil, false
	}
	var format struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(text.Format, &format); err != nil {
		return nil, false
	}
	if format.Type == "" || format.Type == "text" {
		return nil, false
	}
	return map[string]any{"type": format.Type}, true
}

// responsesInputToChat instructions + input → chat messages。
func responsesInputToChat(instructions string, inputRaw json.RawMessage) ([]map[string]any, error) {
	var out []map[string]any
	if instructions != "" {
		out = append(out, map[string]any{"role": "system", "content": instructions})
	}

	// input 为纯字符串 → 单条 user。
	var s string
	if err := json.Unmarshal(inputRaw, &s); err == nil {
		if strings.TrimSpace(s) == "" && len(out) == 0 {
			return nil, fmt.Errorf("input is empty")
		}
		if strings.TrimSpace(s) != "" {
			out = append(out, map[string]any{"role": "user", "content": s})
		}
		return out, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil, fmt.Errorf("input: unsupported shape (neither string nor item array)")
	}

	// pendingReasoning：reasoning item 的文本，挂到下一个 assistant 消息
	// （DeepSeek 思考回放：与工具调用同行的 reasoning_content）。
	pendingReasoning := ""

	for i, raw := range items {
		var head struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("input[%d]: %w", i, err)
		}
		switch head.Type {
		case "", "message":
			msgs, reasoningUsed, err := responsesMessageItemToChat(raw, pendingReasoning)
			if err != nil {
				return nil, fmt.Errorf("input[%d]: %w", i, err)
			}
			if reasoningUsed {
				pendingReasoning = ""
			}
			out = append(out, msgs...)
		case "function_call":
			msg, err := responsesFunctionCallItemToChat(raw)
			if err != nil {
				return nil, fmt.Errorf("input[%d]: %w", i, err)
			}
			// reasoning 挂到带 tool_calls 的 assistant 消息。
			if pendingReasoning != "" {
				msg["reasoning_content"] = pendingReasoning
				pendingReasoning = ""
			}
			out = append(out, msg)
		case "function_call_output":
			msg, err := responsesFunctionCallOutputItemToChat(raw)
			if err != nil {
				return nil, fmt.Errorf("input[%d]: %w", i, err)
			}
			out = append(out, msg)
		case "reasoning":
			if text := responsesReasoningText(raw); text != "" {
				pendingReasoning = text
			}
		case "web_search_call", "local_shell_call", "custom_tool_call", "custom_tool_call_output":
			// 服务器/自定义工具 item（本网关不发起；历史回放时出现）：文本占位。
			out = append(out, map[string]any{
				"role":    "user",
				"content": fmt.Sprintf("[%s item omitted]", head.Type),
			})
		default:
			// 未知 item 类型忽略（前向兼容）。
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no convertible content in input")
	}
	return out, nil
}

// responsesMessageItemToChat message item → chat message(s)。
// content 可能是 string 或 parts[]（input_text/output_text/input_image）。
// 返回 (messages, reasoningUsed)。reasoningUsed：assistant 消息已消费
// pendingReasoning（仅当消息带 tool_calls 或有正文——对齐 sub2api 语义：
// 普通文本轮不需要回放思考）。
func responsesMessageItemToChat(raw json.RawMessage, pendingReasoning string) ([]map[string]any, bool, error) {
	var item struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, false, err
	}
	role := item.Role
	switch role {
	case "user", "assistant", "system", "developer":
		if role == "developer" {
			role = "system"
		}
	default:
		role = "user"
	}

	// content 纯字符串。
	var s string
	if err := json.Unmarshal(item.Content, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, false, nil
		}
		return []map[string]any{{"role": role, "content": s}}, false, nil
	}

	// content parts[]。
	var parts []json.RawMessage
	if err := json.Unmarshal(item.Content, &parts); err != nil {
		return nil, false, fmt.Errorf("content: unsupported shape")
	}
	var textParts []string
	var imageParts []map[string]any
	for _, p := range parts {
		var head struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(p, &head); err != nil {
			continue
		}
		switch head.Type {
		case "input_text", "output_text":
			if head.Text != "" {
				textParts = append(textParts, head.Text)
			}
		case "input_image":
			var img struct {
				ImageURL string `json:"image_url"`
			}
			if err := json.Unmarshal(p, &img); err == nil && img.ImageURL != "" {
				imageParts = append(imageParts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": img.ImageURL},
				})
			}
		case "summary_text":
			// reasoning 摘要片段：跳过（无摘要通道）。
		}
	}

	if len(imageParts) > 0 {
		var content []map[string]any
		for _, t := range textParts {
			content = append(content, map[string]any{"type": "text", "text": t})
		}
		content = append(content, imageParts...)
		return []map[string]any{{"role": role, "content": content}}, false, nil
	}
	if len(textParts) == 0 {
		return nil, false, nil
	}
	msg := map[string]any{"role": role, "content": strings.Join(textParts, "\n\n")}
	return []map[string]any{msg}, false, nil
}

// responsesFunctionCallItemToChat function_call item → assistant tool_calls 消息。
func responsesFunctionCallItemToChat(raw json.RawMessage) (map[string]any, error) {
	var item struct {
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	args := item.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		args = "{}"
	}
	return map[string]any{
		"role": "assistant",
		"content": nil,
		"tool_calls": []map[string]any{{
			"id":   item.CallID,
			"type": "function",
			"function": map[string]any{
				"name":      item.Name,
				"arguments": args,
			},
		}},
	}, nil
}

// responsesFunctionCallOutputItemToChat function_call_output → tool role 消息。
func responsesFunctionCallOutputItemToChat(raw json.RawMessage) (map[string]any, error) {
	var item struct {
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"` // string 或 {content:[...]} 形态
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	output := responsesToolOutputText(item.Output)
	return map[string]any{
		"role":         "tool",
		"content":      output,
		"tool_call_id": item.CallID,
	}, nil
}

// responsesToolOutputText 提取 function_call_output.output 的文本。
// string 直取；对象形态取 content[].text。
func responsesToolOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		var parts []string
		for _, c := range obj.Content {
			if (c.Type == "output_text" || c.Type == "input_text") && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return string(raw)
}

// responsesReasoningText 提取 reasoning item 的文本（summary[].text 或裸 text）。
func responsesReasoningText(raw json.RawMessage) string {
	var item struct {
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &item); err == nil {
		var parts []string
		for _, s := range item.Summary {
			if s.Text != "" {
				parts = append(parts, s.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	var flat struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat.Text
	}
	return ""
}

// responsesToolsToChat 工具转换：仅 function 类型；web_search/custom 等
// 第一版丢弃。
func responsesToolsToChat(tools []ResponsesTool) ([]map[string]any, map[string]bool) {
	var out []map[string]any
	declared := map[string]bool{}
	for _, t := range tools {
		if t.Type != "function" || t.Name == "" {
			continue
		}
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		fn := map[string]any{
			"name":       t.Name,
			"parameters": schema,
		}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
		declared[t.Name] = true
	}
	return out, declared
}

// responsesToolChoiceToChat tool_choice 映射（Responses 的字符串形态直接
// 是 "auto"/"none"/"required"；对象形态 {type:"function",name}）。
func responsesToolChoiceToChat(raw json.RawMessage, declared map[string]bool) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s, true
		}
		return nil, false
	}
	var tc struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, false
	}
	if tc.Type == "function" && tc.Function.Name != "" && declared[tc.Function.Name] {
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Function.Name},
		}, true
	}
	return nil, false
}
