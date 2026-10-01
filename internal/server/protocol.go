// protocol.go 入站协议端点：/v1/messages（Anthropic）与 /v1/responses
// （OpenAI Responses）。
//
// 架构：协议入口把入站 body 转成 Chat Completions body，构造新请求直接调
// h.chatCompletions（100% 复用：选号/轮转/错误分类/限额/并发槽/兜底/记账，
// 该函数零改动），出口用 convertingWriter 拦截 Chat 输出并转回入站协议：
//   - 流式：按 "data: {...}" 行切分喂状态机，事件即时写出（SSE 实时性保持）
//   - 非流式：缓冲完整 JSON 后转换写回
//   - 错误响应（≥400 的 OpenAI error JSON）：转为入站协议错误格式
//
// 兜底渠道（ContentFallback.forward 直写 w）也经过本 wrapper，天然转换。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apicompat"
)

// protoKind 入站协议类型。
type protoKind int

const (
	protoAnthropic protoKind = iota // /v1/messages
	protoResponses                  // /v1/responses
)

// ---------------------------------------------------------------------------
// 端点 handler
// ---------------------------------------------------------------------------

// anthropicMessages POST /v1/messages：入站 Anthropic Messages。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	h.protocolEntry(w, r, protoAnthropic)
}

// openaiResponses POST /v1/responses：入站 OpenAI Responses。
func (h *Handler) openaiResponses(w http.ResponseWriter, r *http.Request) {
	h.protocolEntry(w, r, protoResponses)
}

// protocolEntry 协议入口共用：转换 body → 构造 chat 请求 → wrapping → 复用
// chatCompletions。
func (h *Handler) protocolEntry(w http.ResponseWriter, r *http.Request, proto protoKind) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeProtocolError(w, proto, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}

	var (
		chatBody []byte
		stream   bool
		convErr  error
	)
	switch proto {
	case protoAnthropic:
		chatBody, stream, convErr = apicompat.AnthropicToChat(body)
	case protoResponses:
		chatBody, stream, convErr = apicompat.ResponsesToChat(body)
	}
	if convErr != nil {
		writeProtocolError(w, proto, http.StatusBadRequest, "invalid_request_error", convErr.Error())
		return
	}

	// 构造 chat 请求：同 ctx（keyID 归因传递）、同 header（clientIP 提取等），
	// body 换 Chat 形态。直接调 chatCompletions（不经 mux，不重复鉴权——
	// 外层 withAuth 已验证）。
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	r2.Header.Set("Content-Type", "application/json")

	cw := newConvertingWriter(w, proto, stream)
	h.chatCompletions(cw, r2)
	cw.Close()
}

// writeProtocolError 协议入口侧错误（转换失败等）：入站协议错误格式。
func writeProtocolError(w http.ResponseWriter, proto protoKind, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body []byte
	switch proto {
	case protoAnthropic:
		body = apicompat.AnthropicErrorJSON(errType, msg)
	default:
		// Responses 错误即 OpenAI 形态。
		body, _ = json.Marshal(map[string]any{
			"error": map[string]any{"message": msg, "type": errType, "code": nil},
		})
	}
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// convertingWriter：拦截 chatCompletions 的 Chat 输出并转为入站协议
// ---------------------------------------------------------------------------

// convertingWriter 实现 http.ResponseWriter + http.Flusher：
//   - 流式模式：按行切 SSE data: 行喂状态机，事件立即写出（保持 SSE 实时推送）；
//     "[DONE]" 哨兵触发 Finalize；上游 error 帧（无 choices 的 data JSON 带
//     error 字段）透传为协议错误。
//   - 非流式模式：缓冲全部字节，Close() 时整包转换写回。
//   - 错误响应（WriteHeader >= 400）：缓冲 body，Close() 时按协议转错误格式
//     写回（Anthropic 转 {"type":"error"}；Responses 本就是 OpenAI 形态透传）。
type convertingWriter struct {
	real   http.ResponseWriter
	proto  protoKind
	stream bool

	headerWritten bool
	status        int
	isError       bool

	// 流式状态
	lineBuf      []byte // 跨 Write 的半行缓冲
	streamFinalized bool
	anthropicSM  *apicompat.AnthropicStreamConverter
	responsesSM  *apicompat.ResponsesStreamConverter

	// 非流式 / 错误缓冲
	bodyBuf bytes.Buffer
}

func newConvertingWriter(w http.ResponseWriter, proto protoKind, stream bool) *convertingWriter {
	cw := &convertingWriter{real: w, proto: proto, stream: stream}
	switch proto {
	case protoAnthropic:
		cw.anthropicSM = apicompat.NewAnthropicStreamConverter("")
	case protoResponses:
		cw.responsesSM = apicompat.NewResponsesStreamConverter("")
	}
	return cw
}

func (cw *convertingWriter) Header() http.Header { return cw.real.Header() }

func (cw *convertingWriter) WriteHeader(status int) {
	if cw.headerWritten {
		return
	}
	cw.headerWritten = true
	cw.status = status
	if status >= 400 {
		cw.isError = true
		// 错误：先缓冲 body（Close 时转换格式再写），头也延迟到 Close。
		return
	}
	// 成功路径：头按模式直接写。
	if cw.stream {
		cw.real.Header().Set("Content-Type", "text/event-stream")
		cw.real.Header().Set("Cache-Control", "no-cache")
		cw.real.Header().Set("Connection", "keep-alive")
	}
	cw.real.WriteHeader(status)
}

func (cw *convertingWriter) Write(p []byte) (int, error) {
	if cw.isError || !cw.stream {
		// 错误体 / 非流式响应：缓冲。
		return cw.bodyBuf.Write(p)
	}
	if !cw.headerWritten {
		cw.WriteHeader(http.StatusOK)
	}
	// 流式：按行切分（SSE 行以 \n 结束），半行留缓冲。
	cw.lineBuf = append(cw.lineBuf, p...)
	for {
		idx := bytes.IndexByte(cw.lineBuf, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(cw.lineBuf[:idx]), "\r")
		cw.lineBuf = cw.lineBuf[idx+1:]
		cw.handleSSELine(line)
	}
	return len(p), nil
}

// handleSSELine 处理一行（可能 "data: {...}" / "data: [DONE]" / 空行 / 注释）。
func (cw *convertingWriter) handleSSELine(line string) {
	if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
		return
	}
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		if p2, ok2 := strings.CutPrefix(line, "data:"); ok2 {
			payload, ok = p2, true
		}
	}
	if !ok {
		return
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		cw.finalizeStream()
		return
	}

	var chunk map[string]any
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return // 非法帧忽略（上游脏数据不毒化协议流）
	}

	// 上游 error 帧（SSE 中途错误，200 已开流）：转协议错误事件后收尾。
	if errObj, ok := chunk["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		if cw.proto == protoAnthropic {
			ev := apicompat.AnthropicStreamEvent{
				Event: "error",
				Data:  map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg}},
			}
			_, _ = cw.real.Write(apicompat.FormatAnthropicSSE(ev))
		} else {
			ev, _ := json.Marshal(map[string]any{"type": "error", "code": "upstream_error", "message": msg})
			_, _ = cw.real.Write(append(append([]byte("data: "), ev...), '\n', '\n'))
		}
		cw.finalizeStream()
		return
	}

	switch cw.proto {
	case protoAnthropic:
		for _, ev := range cw.anthropicSM.Chunk(chunk) {
			_, _ = cw.real.Write(apicompat.FormatAnthropicSSE(ev))
		}
	case protoResponses:
		for _, ev := range cw.responsesSM.Chunk(chunk) {
			_, _ = cw.real.Write(apicompat.FormatResponsesSSE(ev))
		}
	}
	if f, ok := cw.real.(http.Flusher); ok {
		f.Flush()
	}
}

// finalizeStream 流收尾：状态机 Finalize 事件（状态机内部幂等）+ 终止行。
// Anthropic 的 message_stop 已含在 Finalize 事件里（无 [DONE] 哨兵，官方协议
// 以 message_stop 事件收尾）；Responses 官方流以 response.completed 后跟
// data: [DONE] 收尾。本方法幂等（可能被 [DONE] 行与 Close 各调一次）。
func (cw *convertingWriter) finalizeStream() {
	if cw.streamFinalized {
		return
	}
	cw.streamFinalized = true
	switch cw.proto {
	case protoAnthropic:
		for _, ev := range cw.anthropicSM.Finalize() {
			_, _ = cw.real.Write(apicompat.FormatAnthropicSSE(ev))
		}
	case protoResponses:
		for _, ev := range cw.responsesSM.Finalize() {
			_, _ = cw.real.Write(apicompat.FormatResponsesSSE(ev))
		}
		_, _ = cw.real.Write([]byte("data: [DONE]\n\n"))
	}
	if f, ok := cw.real.(http.Flusher); ok {
		f.Flush()
	}
}

// Close 收尾：非流式 / 错误路径在此转换写回；流式路径幂等无操作。
func (cw *convertingWriter) Close() {
	// 流式正常结束（[DONE] 已处理）：无操作。流式中断（客户端断连/上游异常，
	// chatCompletions 直接 return）：补发 Finalize 保证协议流完整性。
	if cw.stream && !cw.isError {
		if !cw.headerWritten {
			// 从未写出任何东西（如轮转全失败但走流式分支前的错误路径由
			// isError 覆盖；此处防御）：无操作。
			return
		}
		cw.finalizeStream()
		return
	}

	body := cw.bodyBuf.Bytes()
	status := cw.status
	if status == 0 {
		status = http.StatusOK
	}

	if cw.isError {
		// 错误体：转协议错误格式写回。
		var converted []byte
		if cw.proto == protoAnthropic {
			converted = apicompat.OpenAIErrorToAnthropic(body)
		} else {
			converted = body // Responses 错误本就是 OpenAI 形态
		}
		cw.real.Header().Set("Content-Type", "application/json")
		cw.real.WriteHeader(status)
		_, _ = cw.real.Write(converted)
		return
	}

	// 非流式成功：整包转换。
	var converted []byte
	if cw.proto == protoAnthropic {
		converted, _ = apicompat.ChatJSONToAnthropic(body, "")
	} else {
		converted, _ = apicompat.ChatJSONToResponses(body, "")
	}
	if converted == nil {
		converted = body // 转换失败防御：透传原文
	}
	cw.real.Header().Set("Content-Type", "application/json")
	if !cw.headerWritten {
		cw.real.WriteHeader(status)
	}
	_, _ = cw.real.Write(converted)
}

// Flush 透传（SSE 推送）。
func (cw *convertingWriter) Flush() {
	if f, ok := cw.real.(http.Flusher); ok {
		f.Flush()
	}
}
