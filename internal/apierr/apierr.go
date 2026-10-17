// Package apierr 把内部错误统一映射为对外 HTTP 语义。
//
// 网关对外暴露两套协议（OpenAI 与 Anthropic），同一个内部错误在两套协议里
// 需要不同的 error.type，但 HTTP 状态码是共通的。把三者的映射集中在一个
// 地方，避免 chat.go / anthropic.go 各维护一份表、改一处漏一处。
//
// 设计要点：映射是「内部错误 → 对外表述」的纯函数，不依赖任何 HTTP 上下文，
// 因此可以被单元测试完整覆盖。
package apierr

import (
	"errors"
	"net/http"

	"codearts2api/internal/codearts"
)

// NoAccount 表示账号池为空（需要用户先去控制台导入 / 登录）。
var NoAccount = errors.New("没有可用账号，请先在控制台「导入本机桌面端」或在 CodeArts Agent 里登录")

// Error 是一次失败的对外表述。
type Error struct {
	// Status 是对外的 HTTP 状态码。
	Status int
	// OpenAI 是 OpenAI 风格的 error.type。
	OpenAI string
	// Anthropic 是 Anthropic 风格的 error.type。
	Anthropic string
}

// From 把内部错误映射为对外表述。
//
// 映射表（与 README「错误语义」一节一一对应）：
//
//	情况                        状态码  OpenAI type            Anthropic type
//	账号池为空                   503     server_error         overloaded_error
//	凭据失效（无法续期）          502     credentials_error    authentication_error
//	上游并发上限 / 429           429     rate_limit_error     rate_limit_error
//	参数或模型名不合法            400     invalid_request_error invalid_request_error
//	模型不存在                   404     invalid_request_error invalid_request_error
//	上游 5xx / 网络错误           502     upstream_error       overloaded_error
//
// 「凭据失效」必须与上游临时故障区分开：前者重试没有意义，只能重新登录；
// 把它报成 overloaded_error 会让客户端一直退避重试。
func From(err error) Error {
	if err == nil {
		return Error{Status: http.StatusOK}
	}
	if errors.Is(err, NoAccount) {
		return Error{Status: http.StatusServiceUnavailable, OpenAI: "server_error", Anthropic: "overloaded_error"}
	}

	var ue *codearts.UpstreamError
	if !errors.As(err, &ue) {
		// 凭据不可用、网络错误等：都归为网关侧问题（502），
		// 但凭据失效要给客户端一个「别重试、去登录」的明确信号。
		if codearts.IsCredentialsError(err) {
			return Error{Status: http.StatusBadGateway, OpenAI: "credentials_error", Anthropic: "authentication_error"}
		}
		return Error{Status: http.StatusBadGateway, OpenAI: "upstream_error", Anthropic: "overloaded_error"}
	}

	switch {
	case codearts.IsTransientUpstream(err):
		// 上游 5xx 用 502；并发上限（上游用 400 表达）与 429 用 429，
		// 让客户端知道「等一会再试」而不是「请求错了」。
		if ue.Status >= 500 {
			return Error{Status: http.StatusBadGateway, OpenAI: "upstream_error", Anthropic: "overloaded_error"}
		}
		return Error{Status: http.StatusTooManyRequests, OpenAI: "rate_limit_error", Anthropic: "rate_limit_error"}
	case ue.Status == http.StatusNotFound:
		return Error{Status: http.StatusNotFound, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}
	case ue.Status == http.StatusOK:
		// 上游用「HTTP 200 + error_code」表达的业务错误（未知模型、参数非法…），
		// 属于客户端问题，不应报成网关故障。
		return Error{Status: http.StatusBadRequest, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}
	case ue.Status >= 400 && ue.Status < 500:
		return Error{Status: http.StatusBadRequest, OpenAI: "invalid_request_error", Anthropic: "invalid_request_error"}
	default:
		return Error{Status: http.StatusBadGateway, OpenAI: "upstream_error", Anthropic: "overloaded_error"}
	}
}
