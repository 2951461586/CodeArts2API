package codearts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// AggregateSSE 把上游的 OpenAI SSE 分片聚合为一个非流式 chat.completion 响应。
//
// 用途：上游对个别模型（实测 glm-5.3）的非流式接口会返回一个**空 body 的 400**，
// 只有流式可用。网关检测到这种情况后自动改用流式请求，再用本函数把分片拼回
// 一次性响应，对客户端保持「非流式」语义。
func AggregateSSE(chunks [][]byte) ([]byte, error) {
	if len(chunks) == 0 {
		return nil, fmt.Errorf("上游没有返回任何分片")
	}

	type toolCall struct {
		ID        string
		Type      string
		Name      string
		Arguments strings.Builder
	}

	var (
		id      string
		model   string
		created int64
		content strings.Builder
		think   strings.Builder
		finish  string
		usage   json.RawMessage
		calls   = map[int]*toolCall{}
		order   []int
	)

	for _, raw := range chunks {
		var c struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		if id == "" {
			id = c.ID
		}
		if model == "" {
			model = c.Model
		}
		if created == 0 {
			created = c.Created
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
			think.WriteString(ch.Delta.ReasoningContent)
			for _, tc := range ch.Delta.ToolCalls {
				call, ok := calls[tc.Index]
				if !ok {
					call = &toolCall{}
					calls[tc.Index] = call
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Type != "" {
					call.Type = tc.Type
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
				}
				call.Arguments.WriteString(tc.Function.Arguments)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
		if len(c.Usage) > 0 && string(c.Usage) != "null" {
			usage = c.Usage
		}
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if think.Len() > 0 {
		msg["reasoning_content"] = think.String()
	}
	if len(order) > 0 {
		sort.Ints(order)
		list := make([]map[string]any, 0, len(order))
		for _, idx := range order {
			call := calls[idx]
			callID := call.ID
			if callID == "" {
				callID = "call_" + RandomHex(12)
			}
			typ := call.Type
			if typ == "" {
				typ = "function"
			}
			list = append(list, map[string]any{
				"index": idx,
				"id":    callID,
				"type":  typ,
				"function": map[string]any{
					"name":      call.Name,
					"arguments": call.Arguments.String(),
				},
			})
		}
		msg["tool_calls"] = list
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	if id == "" {
		id = "chatcmpl-" + RandomHex(12)
	}

	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if len(usage) > 0 {
		resp["usage"] = usage
	}
	return json.Marshal(resp)
}

// UpstreamBusinessError 把「HTTP 200 但业务失败」包装成可分类的 UpstreamError。
//
// status 是业务状态码（见 statusFromCode），用于区分「请求有问题」和「账号有问题」。
func UpstreamBusinessError(status int, msg string) error {
	if status <= 0 {
		status = http.StatusBadRequest
	}
	return &UpstreamError{Status: status, Message: msg}
}

// transientCodes 是「换个时机重试即可」的上游业务码，不应计为账号故障。
//
// TM.00001041 是实测到的并发会话上限（每个账号 3 个并发会话）。
var transientCodes = []string{
	"TM.00001041",
}

// asUpstreamError 取出错误链里的 *UpstreamError，取不到返回 nil。
//
// 所有分类函数都走它，而不是裸类型断言：只要有一层包装（fmt.Errorf("...: %w")），
// 裸断言就会静默返回「不是上游错误」，把整个分类逻辑短路 —— 账号又被误标，
// 或者临时故障不再重试，都不会有任何报错。
func asUpstreamError(err error) *UpstreamError {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue
	}
	return nil
}

// IsTransientUpstream 报告一个上游错误是否属于临时性故障。
//
// 临时性故障（并发上限、429、5xx）换账号或稍后重试即可，
// 不应该把账号标记为「不可用」—— 否则一次并发打满就会让健康账号被熔断。
func IsTransientUpstream(err error) bool {
	ue := asUpstreamError(err)
	if ue == nil {
		return false
	}
	if ue.Status == http.StatusTooManyRequests || ue.Status >= 500 {
		return true
	}
	for _, code := range transientCodes {
		if strings.Contains(ue.Message, code) {
			return true
		}
	}
	return false
}

// clientRequestStatuses 是「请求本身有问题」会用到的状态码。
//
// 400 参数越界 / 422 语义不合法 / 413 体积超限，三者都只能由客户端改请求解决。
// 401 / 403 / 404 刻意不在此列：它们更可能是账号或上游侧的问题。
var clientRequestStatuses = []int{
	http.StatusBadRequest,
	http.StatusRequestEntityTooLarge,
	http.StatusUnprocessableEntity,
}

// credentialCodes 是「账号侧」的签名 / 凭据类错误码。
//
// 上游有时把它们放在 HTTP 400 的业务错误里返回。这类错误换账号或重新登录才有意义，
// 绝不能因为状态码是 4xx 就被当成「客户端请求非法」而放过 —— 那会让真正失效的
// 账号继续留在池子里，每次都白试一次。
var credentialCodes = []string{
	"APIG.0301", "APIG.0303",
	"STS5.1804", "STS5.1806",
	"invalid_grant",
}

// IsClientRequestError 报告这个上游错误是否由**请求本身**引起，与账号无关。
//
// 上游对参数非法（max_tokens 越界、temperature 越界、tools 超长等）会返回
// 业务码 400 / 413 / 422。这类错误换哪个账号都一样，绝不能计入账号故障 ——
// 否则任何一个客户端发错一次参数，就会把健康账号标成 error 并累加失败数。
//
// 判定是「状态码 + 排除项」，不是只看 4xx：
//   - 400 / 413 / 422 默认归为请求问题；
//   - 但「空 body 的 400」是「该模型只支持流式」，有自己的重试路径，不是参数问题；
//   - 凭据 / 签名类错误码哪怕用 400 表达，也必须计入账号故障。
func IsClientRequestError(err error) bool {
	ue := asUpstreamError(err)
	if ue == nil {
		return false
	}
	matched := false
	for _, status := range clientRequestStatuses {
		if ue.Status == status {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	if IsEmptyRejection(ue) {
		return false
	}
	for _, code := range credentialCodes {
		if strings.Contains(ue.Message, code) {
			return false
		}
	}
	return true
}

// IsEmptyRejection 报告一个上游错误是否是「空 body 的 4xx」。
//
// 上游对只支持流式的模型会这样回：HTTP 400，body 长度为 0。
// 这类错误值得用流式重试一次。
func IsEmptyRejection(err error) bool {
	ue := asUpstreamError(err)
	if ue == nil {
		return false
	}
	return ue.Status >= 400 && ue.Status < 500 && strings.TrimSpace(ue.Message) == ""
}
