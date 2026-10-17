// Package protocol 负责 Anthropic Messages API 与 OpenAI Chat Completions
// 之间的**纯数据转换**。
//
// 这里不碰 HTTP、不写响应：handler 与 SSE 流式编码留在 internal/server。
// 这样拆分的好处是转换逻辑可以脱离网络被完整单测（见 anthropic_test.go），
// 同时 server 不再是一个既做传输、又做协议适配的上帝包。
package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request 是 Anthropic Messages 请求体中网关关心的字段。
type Request struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []Message       `json:"messages"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Tools         []Tool          `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
}

// Message 是一条 Anthropic 消息（content 为字符串或内容块数组）。
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Tool 是 Anthropic 的工具声明。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Reply 是转换后的 Anthropic message 及其文本统计。
//
// Text / Thinking 供调用方统计输出长度（写日志用），Payload 是最终响应体。
type Reply struct {
	Payload  map[string]any
	Text     string
	Thinking string
}

// --- Anthropic → OpenAI ---

// ToOpenAI 把 Anthropic 请求转换为 OpenAI Chat Completions 请求体。
func ToOpenAI(req *Request, model string) (map[string]any, error) {
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
	if sys := SystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		converted, err := MessageToOpenAI(m)
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
			out["tool_choice"] = ToolChoice(req.ToolChoice)
		}
	}
	return out, nil
}

// ToolChoice 把 Anthropic 的 tool_choice 映射为 OpenAI 形态。
func ToolChoice(raw json.RawMessage) any {
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

// MessageToOpenAI 把一条 Anthropic 消息展开为一条或多条 OpenAI 消息。
//
// tool_use 会变成 assistant.tool_calls，tool_result 会变成独立的 role=tool 消息。
func MessageToOpenAI(m Message) ([]map[string]any, error) {
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
			if u := ImageURL(b); u != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "tool_use":
			args, _ := json.Marshal(b["input"])
			toolCalls = append(toolCalls, map[string]any{
				"id":   Str(b["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      Str(b["name"]),
					"arguments": string(args),
				},
			})
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": Str(b["tool_use_id"]),
				"content":      ToolResultText(b),
			})
		case "thinking", "redacted_thinking":
			// 不回流推理内容：上游不需要，且会浪费上下文
		}
	}

	out := make([]map[string]any, 0, 2)
	msg := map[string]any{"role": role}
	if len(parts) == 1 {
		// 单个文本块直接摊平成字符串，与 OpenAI 的常见写法一致
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

// SystemText 提取 system 字段（字符串或文本块数组）。
func SystemText(raw json.RawMessage) string {
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

// ImageURL 把 Anthropic 图片块转成 data: / http(s) 地址。
func ImageURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok {
		return ""
	}
	switch src["type"] {
	case "base64":
		media := Str(src["media_type"])
		data := Str(src["data"])
		if data == "" {
			return ""
		}
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + data
	case "url":
		return Str(src["url"])
	}
	return ""
}

// ToolResultText 提取工具结果中的文本。
func ToolResultText(block map[string]any) string {
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

// FromOpenAI 把一次非流式 OpenAI 响应映射为 Anthropic message。
func FromOpenAI(raw []byte, model string) Reply {
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
				id = "toolu_" + randomHex(12)
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
		in = EstimatePromptLen(raw)
	}
	out := resp.Usage.CompletionTokens
	if out == 0 {
		out = len([]rune(text)) + len([]rune(thinking))
	}

	return Reply{
		Text:     text,
		Thinking: thinking,
		Payload: map[string]any{
			"id":            "msg_" + randomHex(12),
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

// Str 从 map 中取字符串。
func Str(v any) string {
	s, _ := v.(string)
	return s
}
