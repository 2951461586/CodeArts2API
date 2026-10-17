package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/apierr"
	"codearts2api/internal/codearts"
	"codearts2api/internal/protocol"
	"codearts2api/internal/registry"
)

// Anthropic Messages API 兼容层（HTTP 部分）。
//
// 上游（CodeArts inferhub）本身是 OpenAI 兼容协议，因此这里做的是
// 「Anthropic 请求 → OpenAI 请求」与「OpenAI 响应/SSE → Anthropic 响应/SSE」
// 的双向映射，覆盖 text / thinking / image / tool_use / tool_result。
//
// 纯数据转换在 internal/protocol；本文件只负责 HTTP 传输与 SSE 编码。

// handleAnthropicMessages 是 POST /v1/messages 的入口。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	start := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}
	var req protocol.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	settings := s.Store.Settings()
	modelName := strings.TrimSpace(req.Model)
	if settings.ForceModel != "" {
		modelName = settings.ForceModel
	}
	if modelName == "" {
		modelName = settings.DefaultModel
	}
	model := s.Reg.Resolve(modelName, settings.ModelAliases)
	if model.IsCredit() && !settings.AllowCreditModels {
		writeAnthropicErr(w, http.StatusForbidden, "invalid_request_error",
			"模型 "+model.ID+" 走积分（计费）通道，当前已在控制台关闭。如需使用请开启「允许积分模型」。")
		return
	}

	upReq, err := protocol.ToOpenAI(&req, model.ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	upBody, err := json.Marshal(upReq)
	if err != nil {
		writeAnthropicErr(w, http.StatusInternalServerError, "api_error", "请求体序列化失败")
		return
	}

	key := extractKey(r)
	timeout := time.Duration(settings.RequestTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	promptLen := protocol.EstimatePromptLen(upBody)

	if req.Stream {
		s.streamAnthropic(w, r, upBody, model, key, timeout, start, promptLen)
		return
	}
	s.blockingAnthropic(w, r, upBody, model, key, timeout, start, promptLen)
}

// blockingAnthropic 收集完整回复后按 Anthropic message 结构返回。
func (s *Server) blockingAnthropic(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	var (
		raw    []byte
		outLen int
	)
	acct, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		res, err := s.chatOnce(ctx, acct, model, body)
		if err != nil {
			return err
		}
		raw = res
		return nil
	})
	if err != nil {
		e := apierr.From(err)
		s.record(key, acct, model.ID, false, e.Status, start, promptLen, 0, err)
		writeAnthropicErr(w, e.Status, e.Anthropic, err.Error())
		return
	}

	out := protocol.FromOpenAI(raw, model.ID)
	outLen = len([]rune(out.Text)) + len([]rune(out.Thinking))
	s.record(key, acct, model.ID, false, http.StatusOK, start, promptLen, outLen, nil)
	writeJSON(w, http.StatusOK, out.Payload)
}

// streamAnthropic 把上游 OpenAI SSE 转换为 Anthropic SSE 事件流。
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, body []byte, model registry.Model, key string, timeout time.Duration, start time.Time, promptLen int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicErr(w, http.StatusInternalServerError, "api_error", "服务端不支持流式响应")
		return
	}

	msgID := "msg_" + codearts.RandomHex(12)
	var (
		started   bool
		outLen    int
		streamErr error
	)

	acct, err := s.withAccountRetry(r.Context(), timeout, func(ctx context.Context, acct *codearts.Account) error {
		st := newAnthropicStream(w, flusher, msgID, model.ID, promptLen)
		err := s.streamOnce(ctx, acct, model, body, func(chunk []byte) error {
			if !started {
				h := w.Header()
				h.Set("Content-Type", "text/event-stream; charset=utf-8")
				h.Set("Cache-Control", "no-cache")
				h.Set("Connection", "keep-alive")
				h.Set("X-Accel-Buffering", "no")
				h.Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				started = true
				st.start()
			}
			n, err := st.consume(chunk)
			outLen += n
			return err
		})
		if err == nil {
			st.finish("")
		}
		return err
	})
	streamErr = err

	if !started {
		e := apierr.From(err)
		msg := "全部账号均不可用"
		if err != nil {
			msg = err.Error()
		}
		s.record(key, acct, model.ID, true, e.Status, start, promptLen, outLen, err)
		writeAnthropicErr(w, e.Status, e.Anthropic, msg)
		return
	}
	if streamErr != nil {
		writeSSE(w, flusher, "error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": apierr.From(streamErr).Anthropic, "message": streamErr.Error()},
		})
	}
	s.record(key, acct, model.ID, true, apierr.From(streamErr).Status, start, promptLen, outLen, streamErr)
}

// --- OpenAI SSE → Anthropic SSE ---

// anthropicStream 把上游 OpenAI SSE 分片转换为 Anthropic SSE 事件。
type anthropicStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	msgID   string
	model   string
	inTok   int

	index    int
	kind     string // "" / "thinking" / "text" / "tool_use"
	toolArgs strings.Builder
	toolID   string
	toolName string
	outTok   int
	// sawToolUse 记录是否出现过工具调用，用于决定 stop_reason
	sawToolUse bool
}

func newAnthropicStream(w http.ResponseWriter, f http.Flusher, msgID, model string, promptLen int) *anthropicStream {
	return &anthropicStream{w: w, flusher: f, msgID: msgID, model: model, inTok: promptLen, index: -1}
}

// start 发送 message_start 事件。
func (a *anthropicStream) start() {
	a.send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": a.msgID, "type": "message", "role": "assistant", "model": a.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": a.inTok, "output_tokens": 0},
		},
	})
}

// openBlock 在需要时切换内容块。
func (a *anthropicStream) openBlock(kind string) {
	if a.kind == kind {
		return
	}
	if a.kind != "" {
		a.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": a.index})
	}
	a.index++
	a.kind = kind
	var block map[string]any
	switch kind {
	case "thinking":
		block = map[string]any{"type": "thinking", "thinking": ""}
	case "tool_use":
		block = map[string]any{"type": "tool_use", "id": a.toolID, "name": a.toolName, "input": map[string]any{}}
	default:
		block = map[string]any{"type": "text", "text": ""}
	}
	a.send("content_block_start", map[string]any{
		"type": "content_block_start", "index": a.index, "content_block": block,
	})
}

// consume 处理一个上游分片，返回新增的输出字符数。
func (a *anthropicStream) consume(chunk []byte) (int, error) {
	var c struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunk, &c); err != nil {
		return 0, nil
	}
	n := 0
	for _, ch := range c.Choices {
		if ch.Delta.ReasoningContent != "" {
			a.openBlock("thinking")
			n += len([]rune(ch.Delta.ReasoningContent))
			a.outTok += len([]rune(ch.Delta.ReasoningContent))
			a.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": a.index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ch.Delta.ReasoningContent},
			})
		}
		if ch.Delta.Content != "" {
			a.openBlock("text")
			n += len([]rune(ch.Delta.Content))
			a.outTok += len([]rune(ch.Delta.Content))
			a.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": a.index,
				"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if a.kind != "tool_use" || a.toolID != tc.ID {
				if tc.ID != "" {
					a.toolID = tc.ID
					a.toolName = tc.Function.Name
					a.toolArgs.Reset()
					a.openBlock("tool_use")
				}
			}
			if tc.Function.Arguments != "" {
				a.sawToolUse = true
				a.toolArgs.WriteString(tc.Function.Arguments)
				a.send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": a.index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
			}
		}
	}
	return n, nil
}

// finish 发送收尾事件。stopReason 为空时按是否出现工具调用自动判定。
func (a *anthropicStream) finish(stopReason string) {
	if stopReason == "" {
		if a.sawToolUse {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	if a.kind != "" {
		a.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": a.index})
	}
	a.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": a.outTok},
	})
	a.send("message_stop", map[string]any{"type": "message_stop"})
}

func (a *anthropicStream) send(event string, payload map[string]any) {
	writeSSE(a.w, a.flusher, event, payload)
}

// writeSSE 输出一个 Anthropic SSE 事件。
func writeSSE(w http.ResponseWriter, f http.Flusher, event string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	f.Flush()
}

// writeAnthropicErr 按 Anthropic 的错误结构返回。
func writeAnthropicErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}
