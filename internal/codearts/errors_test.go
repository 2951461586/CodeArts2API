package codearts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestDescribeRefreshError 把上游的续期错误码翻译成人话。
func TestDescribeRefreshError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"refresh_token 已被使用（单次 + 轮转）",
			errors.New(`续期失败 HTTP 400: {"error_code":"STS5.1806","error_msg":"invalid refresh token: 'the refresh token has been used'"}`),
			"refresh_token 已被使用过",
		},
		{"invalid_grant", errors.New(`{"error":"invalid_grant"}`), "refresh_token 已失效或过期"},
		{"STS5.1804", errors.New("STS5.1804 token expired"), "refresh_token 已过期"},
		{"APIG.0301", errors.New("APIG.0301 signature mismatch"), "签名校验失败"},
		{"APIG.0303", errors.New("APIG.0303"), "签名校验失败"},
		{"DPoP", errors.New("DPoP proof invalid"), "DPoP 密钥校验失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DescribeRefreshError(c.err)
			if !strings.Contains(got, c.want) {
				t.Errorf("DescribeRefreshError = %q，期望包含 %q", got, c.want)
			}
			// 原始错误码不该直接丢给用户
			if strings.Contains(got, "error_code") {
				t.Errorf("翻译结果里还带着原始 error_code: %q", got)
			}
		})
	}

	// 未识别的错误原样返回，不吞信息
	other := errors.New("connection reset by peer")
	if got := DescribeRefreshError(other); got != other.Error() {
		t.Errorf("未识别的错误应原样返回，got %q", got)
	}
	if got := DescribeRefreshError(nil); got != "" {
		t.Errorf("nil 应返回空串，got %q", got)
	}
}

// TestCredentialsErrorMessage 错误消息必须告诉用户「怎么办」。
func TestCredentialsErrorMessage(t *testing.T) {
	err := &CredentialsError{Account: "hid_abc", Reason: "自动续期失败：refresh_token 已失效或过期"}
	msg := err.Error()
	for _, want := range []string{"hid_abc", "登录态已失效", "refresh_token 已失效或过期", "重新登录"} {
		if !strings.Contains(msg, want) {
			t.Errorf("消息缺少 %q: %s", want, msg)
		}
	}
}

// TestIsCredentialsError 类型判定与 Unwrap。
func TestIsCredentialsError(t *testing.T) {
	inner := errors.New("boom")
	ce := &CredentialsError{Account: "a", Reason: "r", Err: inner}

	if !IsCredentialsError(ce) {
		t.Error("CredentialsError 本身应被识别")
	}
	if !IsCredentialsError(fmt.Errorf("上层包装: %w", ce)) {
		t.Error("被包装的 CredentialsError 也应被识别")
	}
	if IsCredentialsError(inner) {
		t.Error("普通错误不应被识别为凭据错误")
	}
	if IsCredentialsError(nil) {
		t.Error("nil 不应被识别为凭据错误")
	}
	if !errors.Is(ce, inner) {
		t.Error("Unwrap 应能穿透到原始错误")
	}
}

// TestManagerSessionReturnsCredentialsError 凭据过期且无法续期时，
// 必须返回 CredentialsError（而不是裸的上游错误），上层才能给出可操作提示。
func TestManagerSessionReturnsCredentialsError(t *testing.T) {
	m := NewManager(NewClient())
	acct := &Account{
		ID: "a1", Name: "测试账号",
		// 没有 UserData → 不会去读 state.vscdb（测试环境无 DPAPI）
		Session: &Session{
			AccessKey: "ak", SecretKey: "sk",
			ExpiresAt: time.Now().Add(-time.Hour),
		},
	}

	_, err := m.Session(context.Background(), acct, ResolveOptions{AllowRefresh: false})
	if err == nil {
		t.Fatal("凭据已过期应返回错误")
	}
	if !IsCredentialsError(err) {
		t.Fatalf("应为 CredentialsError，实际 %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "重新登录") {
		t.Errorf("消息应提示重新登录: %v", err)
	}
}

// TestManagerSessionUsesValidCachedSession 未过期的缓存会话应直接可用。
func TestManagerSessionUsesValidCachedSession(t *testing.T) {
	m := NewManager(NewClient())
	sess := &Session{AccessKey: "ak", SecretKey: "sk", ExpiresAt: time.Now().Add(time.Hour)}
	acct := &Account{ID: "a1", Name: "测试账号", Session: sess}

	got, err := m.Session(context.Background(), acct, ResolveOptions{AllowRefresh: false})
	if err != nil {
		t.Fatalf("未过期的会话应可用: %v", err)
	}
	if got != sess {
		t.Error("应返回缓存里的那个会话")
	}
}
