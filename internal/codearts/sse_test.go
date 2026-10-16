package codearts

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAggregateSSEContentAndReasoning(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"id":"as-1","object":"chat.completion.chunk","created":1792125002,"model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"Let"}}]}`),
		[]byte(`{"id":"as-1","object":"chat.completion.chunk","created":1792125002,"model":"glm-5.3","choices":[{"index":0,"delta":{"reasoning_content":" me"}}]}`),
		[]byte(`{"id":"as-1","object":"chat.completion.chunk","created":1792125002,"model":"glm-5.3","choices":[{"index":0,"delta":{"content":"你好"}}]}`),
		[]byte(`{"id":"as-1","object":"chat.completion.chunk","created":1792125002,"model":"glm-5.3","choices":[{"index":0,"delta":{"content":"！"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":5,"total_tokens":22}}`),
	}

	raw, err := AggregateSSE(chunks)
	if err != nil {
		t.Fatalf("AggregateSSE: %v", err)
	}
	var resp struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v", err)
	}

	if resp.Object != "chat.completion" {
		t.Errorf("object = %q，应为 chat.completion", resp.Object)
	}
	if resp.ID != "as-1" || resp.Model != "glm-5.3" || resp.Created != 1792125002 {
		t.Errorf("头部字段未透传: id=%s model=%s created=%d", resp.ID, resp.Model, resp.Created)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices 数量 = %d", len(resp.Choices))
	}
	msg := resp.Choices[0].Message
	if msg.Content != "你好！" {
		t.Errorf("content = %q，应为 %q", msg.Content, "你好！")
	}
	if msg.ReasoningContent != "Let me" {
		t.Errorf("reasoning_content = %q，应为 %q", msg.ReasoningContent, "Let me")
	}
	if msg.Role != "assistant" {
		t.Errorf("role = %q", msg.Role)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage["total_tokens"] != float64(22) {
		t.Errorf("usage 未透传: %v", resp.Usage)
	}
}

func TestAggregateSSEToolCalls(t *testing.T) {
	chunks := [][]byte{
		[]byte(`{"id":"as-2","created":1,"model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`),
		[]byte(`{"id":"as-2","created":1,"model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`),
		[]byte(`{"id":"as-2","created":1,"model":"glm-5.3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}`),
		[]byte(`{"id":"as-2","created":1,"model":"glm-5.3","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}

	raw, err := AggregateSSE(chunks)
	if err != nil {
		t.Fatalf("AggregateSSE: %v", err)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v", err)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数量 = %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call 头部不对: %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"city":"北京"}` {
		t.Errorf("arguments 拼接错误: %q", calls[0].Function.Arguments)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

func TestAggregateSSEEmpty(t *testing.T) {
	if _, err := AggregateSSE(nil); err == nil {
		t.Error("空分片列表应报错，而不是返回空响应")
	}
}

func TestAggregateSSESkipsGarbage(t *testing.T) {
	chunks := [][]byte{
		[]byte(`not json`),
		[]byte(`{"id":"as-3","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`),
	}
	raw, err := AggregateSSE(chunks)
	if err != nil {
		t.Fatalf("AggregateSSE: %v", err)
	}
	if !strings.Contains(string(raw), `"content":"ok"`) {
		t.Errorf("应跳过坏分片并保留好分片: %s", raw)
	}
}

func TestIsEmptyRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"空 body 的 400", &UpstreamError{Status: http.StatusBadRequest, Message: ""}, true},
		{"空 body 的 404", &UpstreamError{Status: http.StatusNotFound, Message: "   "}, true},
		{"有 body 的 400", &UpstreamError{Status: http.StatusBadRequest, Message: "bad"}, false},
		{"空 body 的 500", &UpstreamError{Status: http.StatusInternalServerError, Message: ""}, false},
		{"非上游错误", errOther{}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := IsEmptyRejection(c.err); got != c.want {
			t.Errorf("%s: IsEmptyRejection = %v, want %v", c.name, got, c.want)
		}
	}
}

type errOther struct{}

func (errOther) Error() string { return "other" }

func TestIsTransientUpstream(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"并发上限（实测）", &UpstreamError{Status: http.StatusBadRequest,
			Message: `TM.00001041: 并发会话数已达上限(3个)，请关闭部分会话后重试。`}, true},
		{"429", &UpstreamError{Status: http.StatusTooManyRequests, Message: "slow down"}, true},
		{"503", &UpstreamError{Status: http.StatusServiceUnavailable, Message: "upstream busy"}, true},
		{"未知模型 4005（不是临时故障）", &UpstreamError{Status: http.StatusOK,
			Message: "4005: unsupported model"}, false},
		{"406 模型名不一致（不是临时故障）", &UpstreamError{Status: http.StatusNotAcceptable,
			Message: "InferHub.001001011.406"}, false},
		{"nil", nil, false},
		{"非上游错误", errOther{}, false},
	}
	for _, c := range cases {
		if got := IsTransientUpstream(c.err); got != c.want {
			t.Errorf("%s: IsTransientUpstream = %v, want %v", c.name, got, c.want)
		}
	}
}
