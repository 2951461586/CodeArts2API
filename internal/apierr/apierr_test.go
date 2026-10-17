package apierr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"codearts2api/internal/codearts"
)

// 本文件是「内部错误 → 对外 HTTP 语义」映射的完整回归。
//
// 历史背景：状态码映射以前无条件返回 502，客户端无法区分
// 「稍后重试即可」（并发上限 / 限流）与「请求本身有问题」，也无法对 429 退避；
// 而凭据失效又会被 Anthropic 侧报成 overloaded_error，暗示客户端反复重试。

// TestFromStatus 覆盖状态码映射。
func TestFromStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"成功", nil, http.StatusOK},
		{"没有账号", NoAccount, http.StatusServiceUnavailable},
		{"包装过的没有账号", fmt.Errorf("外层: %w", NoAccount), http.StatusServiceUnavailable},
		{"普通错误", errors.New("boom"), http.StatusBadGateway},
		{
			"并发上限（上游用 400 表达）",
			&codearts.UpstreamError{Status: http.StatusBadRequest, Message: `{"error_code":"TM.00001041"}`},
			http.StatusTooManyRequests,
		},
		{"上游 429", &codearts.UpstreamError{Status: http.StatusTooManyRequests}, http.StatusTooManyRequests},
		{"上游 500", &codearts.UpstreamError{Status: http.StatusInternalServerError}, http.StatusBadGateway},
		{"上游 503", &codearts.UpstreamError{Status: http.StatusServiceUnavailable}, http.StatusBadGateway},
		{"上游 404", &codearts.UpstreamError{Status: http.StatusNotFound}, http.StatusNotFound},
		{
			"上游业务错误（未知模型）",
			&codearts.UpstreamError{Status: http.StatusOK, Message: "4005: unsupported model"},
			http.StatusBadRequest,
		},
		{"上游 403", &codearts.UpstreamError{Status: http.StatusForbidden}, http.StatusBadRequest},
		{"凭据失效", &codearts.CredentialsError{Account: "a", Reason: "已过期"}, http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := From(c.err).Status; got != c.want {
				t.Errorf("From(%v).Status = %d，期望 %d", c.err, got, c.want)
			}
		})
	}
}

// TestFromOpenAIType 保证 OpenAI 侧 error.type 与状态码一致。
func TestFromOpenAIType(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&codearts.UpstreamError{Status: http.StatusBadRequest, Message: "TM.00001041"}, "rate_limit_error"},
		{&codearts.UpstreamError{Status: http.StatusNotFound}, "invalid_request_error"},
		{&codearts.UpstreamError{Status: http.StatusOK, Message: "4005: bad"}, "invalid_request_error"},
		{NoAccount, "server_error"},
		{errors.New("boom"), "upstream_error"},
		// 凭据不可用：重试没意义，必须与上游故障区分开
		{&codearts.CredentialsError{Account: "a", Reason: "已过期"}, "credentials_error"},
		{fmt.Errorf("包装: %w", &codearts.CredentialsError{Account: "a"}), "credentials_error"},
	}
	for _, c := range cases {
		if got := From(c.err).OpenAI; got != c.want {
			t.Errorf("From(%v).OpenAI = %q，期望 %q", c.err, got, c.want)
		}
	}
}

// TestFromAnthropicType 覆盖 Anthropic 侧 error.type。
func TestFromAnthropicType(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&codearts.UpstreamError{Status: http.StatusTooManyRequests}, "rate_limit_error"},
		{&codearts.UpstreamError{Status: http.StatusBadRequest, Message: "TM.00001041"}, "rate_limit_error"},
		{&codearts.UpstreamError{Status: http.StatusNotFound}, "invalid_request_error"},
		{&codearts.UpstreamError{Status: http.StatusOK, Message: "4005: bad"}, "invalid_request_error"},
		{&codearts.UpstreamError{Status: http.StatusInternalServerError}, "overloaded_error"},
		{NoAccount, "overloaded_error"},
		{errors.New("boom"), "overloaded_error"},
		// 凭据失效不能报成 overloaded_error（那会暗示客户端重试）
		{&codearts.CredentialsError{Account: "a", Reason: "已过期"}, "authentication_error"},
	}
	for _, c := range cases {
		if got := From(c.err).Anthropic; got != c.want {
			t.Errorf("From(%v).Anthropic = %q，期望 %q", c.err, got, c.want)
		}
	}
}

// TestFromNeverReturnsEmptyType 保证任何错误都会得到一个可用的 type
// （客户端不会收到空字符串的 error.type）。
func TestFromNeverReturnsEmptyType(t *testing.T) {
	errs := []error{
		nil,
		errors.New("boom"),
		NoAccount,
		&codearts.UpstreamError{Status: http.StatusTeapot},
		&codearts.UpstreamError{Status: http.StatusOK},
		&codearts.CredentialsError{Account: "a"},
	}
	for _, err := range errs {
		got := From(err)
		if err != nil && (got.OpenAI == "" || got.Anthropic == "") {
			t.Errorf("From(%v) 返回空的 error.type: %+v", err, got)
		}
	}
}
