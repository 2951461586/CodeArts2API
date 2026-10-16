package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
)

// Anthropic Messages API 兼容层。
//
// 上游（CodeArts inferhub）本身是 OpenAI 兼容协议，因此这里做的是
// 「Anthropic 请求 → OpenAI 请求」与「OpenAI 响应/SSE → Anthropic 响应/SSE」
// 的双向映射，覆盖 text / thinking / image / tool_use / tool_result。

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

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
	var req anthropicRequest
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

	upReq, err := anthropicToOpenAI(&req, model.ID)
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
	promptLen := estimatePromptLen(upBody)

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
		s.record(key, acct, model.ID, false, http.StatusBadGateway, start, promptLen, 0, err)
		writeAnthropicErr(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	out := openAIToAnthropicMessage(raw, model.ID)
	outLen = len([]rune(out.text)) + len([]rune(out.thinking))
	s.record(key, acct, model.ID, false, http.StatusOK, start, promptLen, outLen, nil)
	writeJSON(w, http.StatusOK, out.payload)
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
		status := http.StatusBadGateway
		msg := "全部账号均不可用"
		if err != nil {
			msg = err.Error()
		}
		s.record(key, acct, model.ID, true, status, start, promptLen, outLen, err)
		writeAnthropicErr(w, status, "api_error", msg)
		return
	}
	if streamErr != nil {
		writeSSE(w, flusher, "error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": streamErr.Error()},
		})
	}
	s.record(key, acct, model.ID, true, statusOf(streamErr), start, promptLen, outLen, streamErr)
}

// --- Anthropic → OpenAI ---

// anthropicToOpenAI 把 Anthropic 请求转换为 OpenAI Chat Completions 请求体。
func anthropicToOpenAI(req *anthropicRequest, model string) (map[string]any, error) {
	out := map[string]any{
		"model":  model,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
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

	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		converted, err := anthropicMessageToOpenAI(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				fn["parameters"] = json.RawMessage(t.InputSchema)
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
		if len(req.ToolChoice) > 0 {
			out["tool_choice"] = anthropicToolChoice(req.ToolChoice)
		}
	}
	return out, nil
}

// anthropicToolChoice 把 Anthropic 的 tool_choice 映射为 OpenAI 形态。
func anthropicToolChoice(raw json.RawMessage) any {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "auto"
	}
	switch obj["type"] {
	case "any":
		return "required"
	case "tool":
		if name, ok := obj["name"].(string); ok {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		return "auto"
	case "none":
		return "none"
	default:
		return "auto"
	}
}

// anthropicMessageToOpenAI 把一条 Anthropic 消息展开为一条或多条 OpenAI 消息。
//
// tool_use 会变成 assistant.tool_calls，tool_result 会变成独立的 role=tool 消息。
func anthropicMessageToOpenAI(m anthropicMessage) ([]map[string]any, error) {
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		return []map[string]any{{"role": m.Role, "content": text}}, nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("content 字段格式不支持")
	}

	role := m.Role
	var (
		parts     []any
		toolCalls []any
		toolMsgs  []map[string]any
	)

	for _, b := range blocks {
		switch b["type"] {
		case "text":
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			if u := anthropicImageURL(b); u != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "tool_use":
			args, _ := json.Marshal(b["input"])
			toolCalls = append(toolCalls, map[string]any{
				"id":   str(b["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      str(b["name"]),
					"arguments": string(args),
				},
			})
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": str(b["tool_use_id"]),
				"content":      anthropicToolResultText(b),
			})
		case "thinking", "redacted_thinking":
			// 不回流推理内容：上游不需要，且会浪费上下文
		}
	}

	out := make([]map[string]any, 0, 2)
	msg := map[string]any{"role": role}
	if len(parts) == 1 {
		if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
			msg["content"] = p["text"]
		} else {
			msg["content"] = parts
		}
	} else if len(parts) > 1 {
		msg["content"] = parts
	} else {
		msg["content"] = ""
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if len(parts) > 0 || len(toolCalls) > 0 {
		out = append(out, msg)
	}
	out = append(out, toolMsgs...)
	return out, nil
}

// anthropicSystemText 提取 system 字段（字符串或文本块数组）。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b["type"] == "text" {
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicImageURL 把 Anthropic 图片块转成 data: / http(s) 地址。
func anthropicImageURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok {
		return ""
	}
	switch src["type"] {
	case "base64":
		media := str(src["media_type"])
		data := str(src["data"])
		if data == "" {
			return ""
		}
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + data
	case "url":
		return str(src["url"])
	}
	return ""
}

// anthropicToolResultText 提取工具结果中的文本。
func anthropicToolResultText(block map[string]any) string {
	switch c := block["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "text" {
					if t, ok := m["text"].(string); ok {
						parts = append(parts, t)
					}
				} else {
					if b, err := json.Marshal(m); err == nil {
						parts = append(parts, string(b))
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	if block["content"] != nil {
		if b, err := json.Marshal(block["content"]); err == nil {
			return string(b)
		}
	}
	return ""
}

// --- OpenAI → Anthropic ---

// anthropicReply 是转换后的 Anthropic message 及其文本统计。
type anthropicReply struct {
	payload  map[string]any
	text     string
	thinking string
}

// openAIToAnthropicMessage 把一次非流式 OpenAI 响应映射为 Anthropic message。
func openAIToAnthropicMessage(raw []byte, model string) anthropicReply {
	var resp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &resp)

	content := make([]map[string]any, 0, 3)
	stopReason := "end_turn"
	var text, thinking string
	if len(resp.Choices) > 0 {
		ch := resp.Choices[0]
		thinking = ch.Message.ReasoningContent
		text = ch.Message.Content
		if thinking != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": thinking})
		}
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, tc := range ch.Message.ToolCalls {
			var input any = map[string]any{}
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			id := tc.ID
			if id == "" {
				id = "toolu_" + codearts.RandomHex(12)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": id, "name": tc.Function.Name, "input": input,
			})
			stopReason = "tool_use"
		}
		if ch.FinishReason == "length" {
			stopReason = "max_tokens"
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	in := resp.Usage.PromptTokens
	if in == 0 {
		in = estimatePromptLen(raw)
	}
	out := resp.Usage.CompletionTokens
	if out == 0 {
		out = len([]rune(text)) + len([]rune(thinking))
	}

	return anthropicReply{
		text:     text,
		thinking: thinking,
		payload: map[string]any{
			"id":            "msg_" + codearts.RandomHex(12),
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       content,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": in, "output_tokens": out},
		},
	}
}

// --- 流式转换器 ---

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

// str 从 map 中取字符串。
func str(v any) string {
	s, _ := v.(string)
	return s
}
