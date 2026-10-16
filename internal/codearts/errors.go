package codearts

import (
	"errors"
	"fmt"
	"strings"
)

// CredentialsError 表示账号凭据不可用：已过期，且没有可用的续期材料或续期失败。
//
// 单独成类型是为了让上层（server）能把它与「上游临时故障」区分开 ——
// 前者必须由用户重新登录才能恢复，退避重试没有任何意义，也不该被
// 当成 overloaded / rate_limit 去误导客户端重试。
type CredentialsError struct {
	// Account 是账号展示名。
	Account string
	// Reason 是一句人话的失败原因（已翻译，可直接展示给用户）。
	Reason string
	// Err 是原始错误，可能为 nil。
	Err error
}

// Error 实现 error。
func (e *CredentialsError) Error() string {
	return fmt.Sprintf(
		"账号 %s 的登录态已失效：%s。请重新登录 CodeArts Agent（或在控制台用「浏览器登录」/「从 session JSON 导入」）",
		e.Account, e.Reason)
}

// Unwrap 暴露原始错误，便于 errors.Is / errors.As 继续向下匹配。
func (e *CredentialsError) Unwrap() error { return e.Err }

// IsCredentialsError 报告错误是否属于「账号凭据不可用」。
func IsCredentialsError(err error) bool {
	var ce *CredentialsError
	return errors.As(err, &ce)
}

// DescribeRefreshError 把续期失败翻译成一句人话。
//
// 与 DescribeClaimError 同样的思路：上游的错误码（STS5.1806 / invalid_grant …）
// 对用户毫无意义，而它们背后的处置动作其实只有一个 —— 重新登录。
func DescribeRefreshError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "STS5.1806"), strings.Contains(msg, "has been used"):
		// refresh_token 是单次使用 + 轮转的：IDE 与网关同时续期时会互相作废
		return "refresh_token 已被使用过（单次有效 + 轮转，IDE 与网关同时续期会互相作废）"
	case strings.Contains(msg, "invalid_grant"):
		return "refresh_token 已失效或过期"
	case strings.Contains(msg, "STS5.1804"):
		return "refresh_token 已过期"
	case strings.Contains(msg, "APIG.0301"), strings.Contains(msg, "APIG.0303"):
		return "签名校验失败，凭据可能已失效"
	case strings.Contains(msg, "DPoP"):
		return "DPoP 密钥校验失败"
	}
	return msg
}
