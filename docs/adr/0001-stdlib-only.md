---
id: adr-0001
type: architecture-design
title: "ADR 0001：只依赖 Go 标准库"
status: active
parent: architecture
---

# ADR 0001：只依赖 Go 标准库

- 状态：已采纳
- 日期：2026-10

## 背景

项目要复用华为云 STS 凭据、做 SDK-HMAC-SHA256 签名、解析 SQLite、读 DPAPI、
起 HTTP/SSE 服务。常见的做法是引入 AWS SDK、`mattn/go-sqlite3`（需要 cgo）、
若干 Web 框架。

## 决策

**不引入任何第三方依赖**，`go.mod` 只有 module / go / toolchain 三行。

- 签名：自己实现（`internal/codearts/signer.go`），并有跨实现的金标准测试；
- SQLite：只读 + 定长原地写，自己解析 B-tree 页（`internal/codearts/sqlite.go`）；
- DPAPI：`syscall` 直接调 Windows API（`session_windows.go`）；
- HTTP/SSE：标准库 `net/http`。

## 后果

### 好处

- 单文件二进制（约 10 MB），`CGO_ENABLED=0` 交叉编译无痛；
- 没有依赖供应链风险 —— 这类工具处理的是账号凭据；
- 构建快、可离线、Docker 镜像小（alpine + ca-certificates）。

### 代价

- 签名、SQLite 解析必须自己写，并自己保证正确性 —— 所以签名器有
  `signer_test.go` 的金标准测试，期望值由独立的 Python 实现算出；
- SQLite 只支持本场景需要的「单表 `ItemTable` 定长读/写」，
  不是通用实现；表结构变化时需要改代码；
- 无框架意味着路由、中间件、参数校验都是手写的（本项目规模下可接受）。

### 何时该推翻

如果上游要求更复杂的 SQLite 操作、或需要完整 OAuth2 客户端能力，
应优先考虑拆出独立的小模块，而不是继续手写。
