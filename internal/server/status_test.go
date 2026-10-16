package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"codearts2api/internal/codearts"
)

// TestStatusOfMapsUpstreamErrors 回归：上游错误不能再被一律压平成 502。
//
// 回归背景：statusOf 以前无条件返回 502，客户端无法区分「稍后重试即可」
// （并发上限 / 限流）与「请求本身有问题」，也无法对 429 做退避。
func TestStatusOfMapsUpstreamErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"成功", nil, http.StatusOK},
		{"没有账号", errNoAccount, http.StatusServiceUnavailable},
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := statusOf(c.err); got != c.want {
				t.Errorf("statusOf = %d，期望 %d", got, c.want)
			}
		})
	}
}

// TestErrorTypeOfMatchesStatus 保证 OpenAI 侧的 error.type 与状态码一致。
func TestErrorTypeOfMatchesStatus(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&codearts.UpstreamError{Status: http.StatusBadRequest, Message: "TM.00001041"}, "rate_limit_error"},
		{&codearts.UpstreamError{Status: http.StatusNotFound}, "invalid_request_error"},
		{&codearts.UpstreamError{Status: http.StatusOK, Message: "4005: bad"}, "invalid_request_error"},
		{errNoAccount, "server_error"},
		{errors.New("boom"), "upstream_error"},
		// 凭据不可用：重试没意义，必须与上游故障区分开
		{&codearts.CredentialsError{Account: "a", Reason: "已过期"}, "credentials_error"},
		{fmt.Errorf("包装: %w", &codearts.CredentialsError{Account: "a"}), "credentials_error"},
	}
	for _, c := range cases {
		if got := errorTypeOf(c.err); got != c.want {
			t.Errorf("errorTypeOf(%v) = %q，期望 %q", c.err, got, c.want)
		}
	}
}

// TestAnthropicErrorType 覆盖 Anthropic 侧的错误 type 映射。
func TestAnthropicErrorType(t *testing.T) {
	cases := map[int]string{
		http.StatusTooManyRequests:     "rate_limit_error",
		http.StatusBadRequest:          "invalid_request_error",
		http.StatusNotFound:            "invalid_request_error",
		http.StatusServiceUnavailable:  "overloaded_error",
		http.StatusBadGateway:          "overloaded_error",
		http.StatusInternalServerError: "api_error",
	}
	for status, want := range cases {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("anthropicErrorType(%d) = %q，期望 %q", status, got, want)
		}
	}
}

// TestAnthropicErrorTypeOfCredentials 凭据失效不应报成 overloaded_error
// （那会暗示客户端重试，而实际只能重新登录）。
func TestAnthropicErrorTypeOfCredentials(t *testing.T) {
	ce := &codearts.CredentialsError{Account: "a", Reason: "已过期"}
	if got := anthropicErrorTypeOf(ce); got != "authentication_error" {
		t.Errorf("anthropicErrorTypeOf(CredentialsError) = %q，期望 authentication_error", got)
	}
	// 其它错误仍走状态码映射
	if got := anthropicErrorTypeOf(errNoAccount); got != "overloaded_error" {
		t.Errorf("anthropicErrorTypeOf(errNoAccount) = %q，期望 overloaded_error", got)
	}
	if got := anthropicErrorTypeOf(&codearts.UpstreamError{Status: http.StatusBadRequest, Message: "TM.00001041"}); got != "rate_limit_error" {
		t.Errorf("并发上限应仍是 rate_limit_error，got %q", got)
	}
}
