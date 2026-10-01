// anthropic_to_chat.go 请求向：Anthropic Messages → OpenAI Chat Completions。
//
// 语义对齐 sub2api chatcompletions_anthropic_bridge（直接桥，无中间表示）：
//   - system（string/blocks）→ 单条 system message
//   - user 的 text/image → user message（纯文本折叠 string，带图才用 parts 数组）
//   - tool_result → 独立 tool role message；其内图片提升为后续 user 图片
//   - assistant 的 text + tool_use → assistant message + tool_calls
//   - thinking → reasoning_content（仅当消息带 tool_calls——DeepSeek 思考回放要求）
package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// chatMessage / chatTool 等出站结构体用 map 形态组装（与网关透传哲学一致，
// 只写需要的键），避免维护一套完整的出站类型系统。

// AnthropicToChat 把 /v1/messages 请求体转换为 Chat Completions 请求体。
// 返回 (chatBody, stream, error)。模型名保留原样（realm 前缀由 server 侧
// resolveModel 剥），max_tokens 写 chat 的 max_tokens（WorkBuddy 上游是宽松
// Chat 方言，既有透传请求都带该字段）。
func AnthropicToChat(body []byte) ([]byte, bool, error) {
	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false, fmt.Errorf("parse messages request: %w", err)
	}
	if req.Model == "" {
		return nil, false, fmt.Errorf("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, false, fmt.Errorf("messages is required")
	}

	out := map[string]any{"model": req.Model}
	if req.Stream {
		out["stream"] = true
	}
	// max_tokens：Anthropic 必填。钳下限防 0（上游 0 = 截断）。
	if req.MaxTokens > 0 {
		out["max_tokens"] = maxTokensClamp(req.MaxTokens)
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}

	messages, err := anthropicMessagesToChat(req.System, req.Messages)
	if err != nil {
		return nil, false, err
	}
	out["messages"] = messages

	// tools：function 才转换；web_search_* 服务器工具丢弃。
	if tools, declared := anthropicToolsToChat(req.Tools); len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := anthropicToolChoiceToChat(req.ToolChoice, declared); ok {
			out["tool_choice"] = tc
		}
	}

	// thinking → reasoning_effort 粗映射（后续走网关 effort 降级管线）。
	if effort, ok := anthropicThinkingToEffort(req.Thinking); ok {
		out["reasoning_effort"] = effort
	}

	chatBody, err := json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	return chatBody, req.Stream, nil
}

// maxTokensClamp Anthropic 上限下限钳制（官方最低 16，对齐 sub2api minMaxOutputTokens）。
func maxTokensClamp(v int) int {
	const minMaxOutputTokens = 16
	if v < minMaxOutputTokens {
		return minMaxOutputTokens
	}
	return v
}

// anthropicThinkingToEffort thinking 开关 → effort 档位粗映射。
// budget_tokens：<4k → low、<16k → medium、其余 → high。disabled → 不带（不思考）。
func anthropicThinkingToEffort(t *AnthropicThinking) (string, bool) {
	if t == nil || t.Type != "enabled" || t.BudgetTokens <= 0 {
		return "", false
	}
	switch {
	case t.BudgetTokens < 4000:
		return "low", true
	case t.BudgetTokens < 16000:
		return "medium", true
	default:
		return "high", true
	}
}

// anthropicSystemToChat 把 system 字段（string 或 blocks）转为 system 文本。
// cache_control 在块级字段上，结构体未声明即自然丢弃。
func anthropicSystemToChat(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("system: unsupported shape: %w", err)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// anthropicMessagesToChat 组装 messages 数组（system + 逐条消息转换）。
func anthropicMessagesToChat(system json.RawMessage, msgs []AnthropicMessage) ([]map[string]any, error) {
	var out []map[string]any

	if sys, err := anthropicSystemToChat(system); err != nil {
		return nil, err
	} else if sys != "" {
		out = append(out, map[string]any{"role": "system", "content": sys})
	}

	for i, m := range msgs {
		converted, err := anthropicMsgToChat(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out = append(out, converted...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no convertible content in messages")
	}
	return out, nil
}

// anthropicMsgToChat 一条 Anthropic 消息 → 一或多条 chat message。
// tool_result 拆独立 tool role；其余内容归并 user/assistant。
func anthropicMsgToChat(m AnthropicMessage) ([]map[string]any, error) {
	// 纯字符串 content 直传。
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []map[string]any{{"role": m.Role, "content": s}}, nil
	}

	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("content: unsupported shape (neither string nor block array)")
	}

	switch m.Role {
	case "assistant":
		return anthropicAssistantBlocksToChat(blocks)
	default:
		return anthropicUserBlocksToChat(blocks)
	}
}

// anthropicUserBlocksToChat user 消息块：tool_result → tool role；text/image →
// user message（纯文本折叠 string，带图才 parts 数组——严格 chat 上游拒绝数组
// content）；tool_result 内图片提升为该 user 消息的图片。
func anthropicUserBlocksToChat(blocks []AnthropicContentBlock) ([]map[string]any, error) {
	var out []map[string]any
	var textParts []string
	var imageParts []map[string]any

	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			text, images, err := toolResultContent(b)
			if err != nil {
				return nil, err
			}
			if b.IsError {
				text = "[tool error] " + text
			}
			out = append(out, map[string]any{
				"role":         "tool",
				"content":      text,
				"tool_call_id": b.ToolUseID,
			})
			imageParts = append(imageParts, images...)
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "image":
			if uri := anthropicImageToDataURI(b.Source); uri != "" {
				imageParts = append(imageParts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": uri},
				})
			}
		case "server_tool_use", "web_search_tool_result":
			// 服务器工具块（本网关不发起，客户端历史回放时出现）：转为文本占位，
			// 上游至少能看到"这里发生过什么"。
			textParts = append(textParts, fmt.Sprintf("[%s block omitted]", b.Type))
		case "thinking", "redacted_thinking":
			// user 消息里的 thinking 块（异常形态）忽略。
		case "document", "audio", "video":
			return nil, fmt.Errorf("content block %q is not supported by this gateway", b.Type)
		default:
			// 未知块类型：宁缺勿错，忽略（前向兼容）。
		}
	}

	if len(imageParts) > 0 {
		var parts []map[string]any
		for _, t := range textParts {
			parts = append(parts, map[string]any{"type": "text", "text": t})
		}
		parts = append(parts, imageParts...)
		out = append(out, map[string]any{"role": "user", "content": parts})
	} else if len(textParts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n\n")})
	}
	return out, nil
}

// anthropicAssistantBlocksToChat assistant 消息块：text → content；tool_use →
// tool_calls；thinking → reasoning_content（仅当带 tool_calls，DeepSeek 思考
// 回放要求；signature 丢弃——非 Claude 上游无校验）。
func anthropicAssistantBlocksToChat(blocks []AnthropicContentBlock) ([]map[string]any, error) {
	var textParts []string
	var toolCalls []map[string]any
	var thinkingParts []string

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				if !json.Valid(b.Input) {
					return nil, fmt.Errorf("tool_use input is not valid JSON")
				}
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		case "thinking":
			if b.Thinking != "" {
				thinkingParts = append(thinkingParts, b.Thinking)
			}
		case "redacted_thinking":
			// 密文无明文贡献，忽略。
		case "server_tool_use", "web_search_tool_result":
			textParts = append(textParts, fmt.Sprintf("[%s block omitted]", b.Type))
		case "image":
			// assistant 消息带图（罕见）：转文本占位。
			textParts = append(textParts, "[assistant image block omitted]")
		case "document", "audio", "video":
			return nil, fmt.Errorf("content block %q is not supported by this gateway", b.Type)
		}
	}

	if len(toolCalls) == 0 && len(textParts) == 0 {
		return nil, nil
	}

	msg := map[string]any{"role": "assistant"}
	if len(textParts) > 0 {
		msg["content"] = strings.Join(textParts, "\n\n")
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		// reasoning_content 只挂在带 tool_calls 的 assistant 消息上（对齐
		// sub2api anthropicThinkingToReasoningContent 的作用域）。
		if len(thinkingParts) > 0 {
			msg["reasoning_content"] = strings.Join(thinkingParts, "\n")
		}
	}
	return []map[string]any{msg}, nil
}

// toolResultContent 提取 tool_result 的内容：string 或块数组。返回文本与
// 其内 image 块转出的 image_url part 列表。
func toolResultContent(b AnthropicContentBlock) (string, []map[string]any, error) {
	if len(b.Content) == 0 {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s, nil, nil
	}
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(b.Content, &blocks); err != nil {
		return "", nil, fmt.Errorf("tool_result content: unsupported shape")
	}
	var texts []string
	var images []map[string]any
	for _, c := range blocks {
		switch c.Type {
		case "text":
			if c.Text != "" {
				texts = append(texts, c.Text)
			}
		case "image":
			if uri := anthropicImageToDataURI(c.Source); uri != "" {
				images = append(images, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": uri},
				})
			}
		}
	}
	return strings.Join(texts, "\n\n"), images, nil
}

// anthropicImageToDataURI 图片源 → data URI。base64 直组；URL 源本网关上游
// 无法按 URL 拉取，转文本占位（不静默丢图）。
func anthropicImageToDataURI(src *AnthropicImageSource) string {
	if src == nil {
		return ""
	}
	if src.Type == "base64" && src.Data != "" {
		media := src.MediaType
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + src.Data
	}
	if src.Type == "url" && src.URL != "" {
		// 无法内联：给上游一个可读占位，避免静默丢内容。
		return ""
	}
	return ""
}

// anthropicToolsToChat 工具定义转换。返回 (chat tools, 已声明名称表)。
func anthropicToolsToChat(tools []AnthropicTool) ([]map[string]any, map[string]bool) {
	var out []map[string]any
	declared := map[string]bool{}
	for _, t := range tools {
		if t.Name == "" || strings.HasPrefix(t.Type, "web_search") {
			continue // 服务器工具无 Chat 等价物，丢弃
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
		declared[t.Name] = true
	}
	return out, declared
}

// anthropicToolChoiceToChat tool_choice 映射：
//
//	{"type":"auto"} → "auto"；{"type":"any"} → "required"；{"type":"none"} → "none"
//	{"type":"tool","name":"X"} → {"type":"function","function":{"name":"X"}}（仅已声明）
func anthropicToolChoiceToChat(raw json.RawMessage, declared map[string]bool) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, false
	}
	switch tc.Type {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		if tc.Name == "" || !declared[tc.Name] {
			return nil, false
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}, true
	default:
		return nil, false
	}
}
