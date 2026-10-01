// errors.go 入站协议的错误响应格式转换。
//
// 网关内部错误（writeOpenAIError / writeOpenAIErrorHint）统一是 OpenAI 形态
// {"error":{"message","type","code"}}：
//   - Anthropic 客户端要求 {"type":"error","error":{"type","message"}}——需要转换；
//   - Responses 客户端（OpenAI 系）本就吃 OpenAI 形态——透传即可。
package apicompat

import "encoding/json"

// AnthropicErrorType 按网关错误 code 映射 Anthropic 错误 type。
//
// Anthropic 官方 type 集：invalid_request_error / authentication_error /
// permission_error / not_found_error / request_too_large / rate_limit_error /
// api_error / overloaded_error。
func AnthropicErrorType(code string) string {
	switch code {
	case "invalid_api_key":
		return "authentication_error"
	case "key_quota_exceeded", "key_concurrency_exceeded", "rate_limit_exceeded":
		return "rate_limit_error"
	case "model_not_found":
		return "not_found_error"
	case "prompt_too_long":
		return "request_too_large"
	case "invalid_request", "image_invalid":
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// OpenAIErrorToAnthropic 把网关的 OpenAI 形态错误 JSON 转为 Anthropic 形态。
// 输入不是合法 OpenAI error 形态时原样返回（防御：上游错误透传路径）。
func OpenAIErrorToAnthropic(body []byte) []byte {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error.Message == "" {
		return body
	}
	out, err := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    AnthropicErrorType(parsed.Error.Code),
			"message": parsed.Error.Message,
		},
	})
	if err != nil {
		return body
	}
	return out
}

// AnthropicErrorJSON 组装一个 Anthropic 错误响应（协议入口侧转换失败用）。
func AnthropicErrorJSON(errType, message string) []byte {
	out, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
	return out
}
