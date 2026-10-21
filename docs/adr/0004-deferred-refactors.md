---
id: adr-0004
type: architecture-design
title: "ADR 0004：有意推迟的重构"
status: active
parent: architecture
---

# ADR 0004：有意推迟的重构

- 状态：已记录（未实施）
- 日期：2026-10

## 背景

架构评审时列出的候选改进不止一项。有些已经落地（见 0002、0003），
另一些**评估后决定暂不做**。记下原因，避免后来者重复讨论，也避免
「以为没人想过」。

## 推迟项

### 1. 拆分 `internal/codearts`

**现状**：11 个导入方，blast radius 15。同一个包里有四类关注点：

| 关注点 | 文件 |
| --- | --- |
| 凭据存储 / 解密 | `session_windows.go`、`session_other.go`、`sqlite.go` |
| 上游传输 | `client.go`、`signer.go`、`sse.go`、`errors.go` |
| 认证流程 | `login.go`、`refresh.go`、`manager.go` |
| 业务功能 | `welfare.go` |

**建议目标**：`internal/credential` / `internal/upstream` /
`internal/authflow` / `internal/benefit`。

> 2026-10 更新：`benefit.go` 已随免费权益通道下线删除（见
> [ADR 0005](0005-remove-benefit-channel.md)），第四类关注点现在只剩 `welfare.go`，
> 「拆 `internal/benefit`」这一项随之取消。

**为什么现在不做**：

- 拆包的前置条件**部分**已满足（2026-10 更新）：`sqlite.go` 的定长原地回写
  已有 `TestPatchItemValueRoundTrip`（覆写后重读一致、长度不一致必须拒绝），
  `refresh.go` 的 DPoP 已有 `TestDPoPProofVerifies`、
  `TestJWKThumbprintMatchesRefreshTokenBinding`，签名器另有
  `TestSignRequestStableVector` 金标准向量。
- 但**剩余缺口仍在**：`sqlite.go` 的读取路径（`scanTree` 的溢出页 / 内部页 / 
  freelist 边界）与 `refresh.go` 的续期失败分支（DPoP 校验失败、refresh_token 过期）
  还没有测试网。在没有这层测试的情况下拆包，等于把「重构错误」和「原有 bug」
  混在一起，出问题无法定位。
- 拆分会产生新的包间依赖（credential 依赖 upstream 的错误类型等），
  需要先定义接口边界，否则只是把耦合换成 import 环。

**建议路径**（下一步）：

1. 补 `sqlite.go` 读取路径（多页 / 溢出页 / 损坏页）与 `refresh.go` 失败分支的测试；
2. 抽出 `internal/credential`（只依赖标准库 + `store` 的错误类型）；
3. 再抽 `internal/upstream`（client + signer + sse + errors）；
4. `welfare` 最后拆，因为它依赖上游客户端。

### 2. `Server` 的依赖接口化

**现状**：`Server` 直接持有 `*store.Store`、`*codearts.Client`、`*registry.Registry`，
测试用 `newTestServer` 构造真实实现（临时状态文件 + 真实 registry）。

**为什么现在不做**：现有测试跑得很快（< 3s），没有必须 mock 的外部 I/O；
接口化要改动所有 handler 的构造与测试脚手架，收益（隔离度）暂时小于成本。
当需要测试「上游返回特定错误序列」这类场景时再做。

### 3. 用 SSE 替代控制台轮询

**现状**：控制台每 20s 拉一次 `/health`，日志页手动刷新。

**为什么现在不做**：浏览器 `EventSource` **无法设置自定义请求头**，
而管理接口的鉴权依赖 `X-Admin-Password` / `Authorization`。只有两条路：

- 把密码放进 query string —— 会进入浏览器历史、代理日志、Referer，**安全降级**；
- 改用 `fetch` + `ReadableStream` 手动解析 SSE —— 复杂度显著上升。

当前轮询的成本（每 20s 一个几 KB 的 JSON）远低于上述代价。

**如果将来要做**：应先加一个「短期一次性 ticket」接口
（`POST /admin/api/events/ticket` 用管理密码换 60 秒有效、一次性、
只能用于 SSE 的 token），再让 `EventSource` 带 ticket 连接。这样既不降级鉴权，
也不需要手写 SSE 客户端。

### 4. `log/slog` 结构化日志

**现状**：30+ 处 `log.Printf`，日志是给人看的文本。

**为什么现在不做**：迁移是机械工作，但要**跑真实上游**才能验证字段与格式；
当前环境无法端到端验证，贸然改会把排障依赖的日志格式打乱。

**建议路径**：与「请求 ID 中间件」一起做 —— 给每个请求生成 id，
在日志里带上 `request_id / account / model / status / latency`，
这样日志才能真正用于追踪。

### 5. golangci-lint 门禁

**现状**：CI 用 `gofmt` + `go vet` + `go test` + `-race` + 控制台检查。
`Makefile` 里保留 `make lint`，但不进 CI。

**为什么现在不做**：golangci-lint 各版本对 Go 1.24 的支持情况不一致，
把 CI 绑到某个版本会引入「工具链版本」这类与代码无关的失败。
等确认一个稳定版本后再加。

## 已落地（对照）

- 控制台拆三文件 + 严格 CSP → ADR 0002
- 协议转换抽 `internal/protocol` + 金标准测试 → ADR 0003
- 错误映射统一到 `internal/apierr`
- 状态文件加 `version` 与迁移
- CI / Makefile / toolchain 固定
- `/metrics`（Prometheus 文本，复用现有统计，不新增计数器）
- `sqlite.go` 定长回写 / DPoP 测试向量（拆分前置条件，见上）
- `chat.go` 主路径测试：退避序列、临时故障不计账号失败、流式→非流式聚合兜底
- 控制台静态契约测试：页签链（TAB_IDS ↔ HTML ↔ TAB_META）、样式类对齐、同源约束
