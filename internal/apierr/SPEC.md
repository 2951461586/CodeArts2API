---
id: apierr
type: module-design
title: apierr —— 内部错误到对外表述的唯一映射点
status: draft
parent: architecture
---

# apierr

## 职责

把内部错误翻译成对外的 HTTP 状态码 + 两套协议的 `error.type`。网关对外同时讲 OpenAI 与
Anthropic，同一个内部错误在两套协议里需要不同的 `type`，但状态码是共通的 —— 这张表只能有一份。

## 边界

- **只依赖 `codearts`**（为了用 `IsCredentialsError` / `IsTransientUpstream` 分类）。
- **不认识 HTTP 上下文**：映射是纯函数，不碰 `*http.Request` / `ResponseWriter`，
  因此可以被单测完整覆盖。

请求处理器**不得**自己拼状态码 —— 改一处漏一处是这类表的天敌。

## 映射

| 情况 | 状态码 | OpenAI type | Anthropic type |
| --- | --- | --- | --- |
| 账号池为空 | 503 | `server_error` | `overloaded_error` |
| 凭据失效（无法续期） | 502 | `credentials_error` | `authentication_error` |
| 网关侧限流（本机密钥打太快） | 429 | `rate_limit_error` | `rate_limit_error` |
| 上游并发上限 / 429 | 429 | `rate_limit_error` | `rate_limit_error` |
| 模型不存在 | 404 | `invalid_request_error` | `invalid_request_error` |
| 参数非法 / 上游 200+业务错 | 400 | `invalid_request_error` | `invalid_request_error` |
| 上游 5xx / 网络错误 | 502 | `upstream_error` | `overloaded_error` |

**凭据失效必须与上游临时故障分开**：前者重试没有意义（只能重新登录），
报成 `overloaded_error` 会让客户端一直退避重试。

**网关侧限流与上游 429 也是两件事**：前者是「这个密钥自己打得太快」，退避时长网关能算准
（`RateLimited.RetryAfter`）；后者是「账号的并发会话满了」，只能让客户端等或换账号。
状态码相同，但错误里的等待时间不能混用。
