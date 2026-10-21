package codearts

import (
	"net/http"
	"strings"
	"testing"
)

// TestStatusFromCode 验证业务错误码能正确映射到 HTTP 状态。
func TestStatusFromCode(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{"400", http.StatusBadRequest},
		{"422", http.StatusUnprocessableEntity},
		{"429", http.StatusTooManyRequests},
		{"500", http.StatusInternalServerError},
		{"503", http.StatusServiceUnavailable},
		// 复合码：取最后一段
		{"InferHub.002002009.404", http.StatusNotFound},
		{"InferHub.001001017.413", 413},
		// 取不到合法状态码时退回 400，绝不返回 200
		{"4005", http.StatusBadRequest},
		{"", http.StatusBadRequest},
		{"abc", http.StatusBadRequest},
		{"200", http.StatusBadRequest}, // 200 不是合法业务错误码
	}
	for _, c := range cases {
		if got := statusFromCode(c.code); got != c.want {
			t.Errorf("statusFromCode(%q) = %d, want %d", c.code, got, c.want)
		}
	}
}

// TestBusinessErrorIsNeverHTTP200 回归：业务错误的 Status 绝不能是 200。
//
// 历史 bug：detectUpstreamError 固定写 Status: http.StatusOK，导致日志里出现
// 自相矛盾的「上游 HTTP 200: The request param is invalid」，且这类错误
// 无法被 IsClientRequestError 识别，进而把健康账号标成 error。
func TestBusinessErrorIsNeverHTTP200(t *testing.T) {
	ue := newBusinessError("400", "The request param is invalid, Please check it")
	if ue.Status == http.StatusOK {
		t.Fatal("业务错误的 Status 不能是 200")
	}
	if strings.Contains(ue.Error(), "HTTP 200") {
		t.Errorf("错误信息不该出现 HTTP 200: %q", ue.Error())
	}
	if !strings.Contains(ue.Error(), "param is invalid") {
		t.Errorf("错误信息丢了原始消息: %q", ue.Error())
	}
}

// TestIsClientRequestError 回归：请求本身非法时不得计入账号故障。
//
// 这直接对应线上现象：客户端发 max_tokens/temperature 越界，上游返回
// HTTP 200 + error_code，网关把两个健康账号都标成 error、failures 一路累加。
func TestIsClientRequestError(t *testing.T) {
	clientErrs := []*UpstreamError{
		{Status: http.StatusBadRequest, Message: "400: The request param is invalid, Please check it"},
		{Status: http.StatusUnprocessableEntity, Message: "422: bad entity"},
		{Status: 413, Message: "InferHub.001001017.413 Function name length over limit"},
		{Status: http.StatusBadRequest, Message: "4005: unsupported model"},
	}
	for _, e := range clientErrs {
		if !IsClientRequestError(e) {
			t.Errorf("%q 应判定为客户端请求错误", e.Error())
		}
		// 客户端错误不能被当成临时故障去换账号重试
		if IsTransientUpstream(e) && e.Status < 500 {
			t.Errorf("%q 不该同时被判为临时故障", e.Error())
		}
	}

	// 账号/服务端问题不能被误判成客户端错误
	serverErrs := []*UpstreamError{
		{Status: http.StatusTooManyRequests, Message: "429 too many requests"},
		{Status: http.StatusServiceUnavailable, Message: "503 auth_unavailable"},
		{Status: http.StatusInternalServerError, Message: "500 internal"},
		{Status: http.StatusUnauthorized, Message: "401 bad signature"},
	}
	for _, e := range serverErrs {
		if IsClientRequestError(e) {
			t.Errorf("%q 不该判定为客户端请求错误", e.Error())
		}
	}
}

// TestIsTransientUpstreamUnaffected 确认临时故障判定没被这次改动影响。
func TestIsTransientUpstreamUnaffected(t *testing.T) {
	if !IsTransientUpstream(&UpstreamError{Status: http.StatusTooManyRequests}) {
		t.Error("429 应判为临时故障")
	}
	if !IsTransientUpstream(&UpstreamError{Status: http.StatusServiceUnavailable}) {
		t.Error("503 应判为临时故障")
	}
	if !IsTransientUpstream(&UpstreamError{Status: http.StatusBadRequest, Message: "TM.00001041 concurrent limit"}) {
		t.Error("并发上限应判为临时故障")
	}
	if IsTransientUpstream(&UpstreamError{Status: http.StatusBadRequest, Message: "param is invalid"}) {
		t.Error("参数错误不该判为临时故障")
	}
}
