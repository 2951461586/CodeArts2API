---
id: architecture
type: architecture-design
title: 分层、模块边界与不变量
status: draft
parent: goal
---

# 架构

单进程 HTTP 服务，没有插件机制、没有队列进程。启动时 `main` 装配状态层与 HTTP 层；
唯一的后台 goroutine 是「启动时刷新模型清单」与「每日领取运营活动」。

## 模块与依赖边

依赖是**单向**的，`go list` 可验证：

| 模块 | 职责 | 依赖 |
| --- | --- | --- |
| `codearts` | 上游复刻层：凭据、签名、请求、续期、活动 | — |
| `protocol` | Anthropic ↔ OpenAI 纯数据转换 | — |
| `ratelimit` | 按密钥的令牌桶 + 在飞并发闸门 | — |
| `apierr` | 内部错误 → 对外状态码与两套 error.type | `codearts` |
| `store` | 状态持久化与账号池调度 | `codearts` |
| `registry` | 模型清单与别名解析 | `codearts` |
| `server` | HTTP 兼容层 + 控制台；**唯一的组合根** | `apierr` `codearts` `protocol` `ratelimit` `registry` `store` |
| `main` | 命令行、启动编排、信号处理 | `codearts` `server` `store` |
| `cmd/probe` | 本机诊断工具 | `codearts` |

## 不变量

代码已经这样长，改架构时别破坏：

1. **`codearts`、`protocol`、`ratelimit` 不依赖任何内部包。** 三者都能脱离网关单测 ——
   前者是上游复刻，后两者是纯逻辑。任何「顺手 import 一下 store」都会毁掉这条。
2. **`server` 是唯一的组合根。** 只有它同时依赖四个下层包。下层之间不互相认识。
3. **对外错误只有一个映射点**：`apierr.From` 决定状态码与两套 `error.type`。
   请求处理器不得自己拼状态码表 —— 改一处漏一处是这类表的天敌。
4. **凭据的失效与上游故障必须分开。** 前者重试无意义（只能重新登录），后者可以退避重试。
   混报会让客户端一直重试到放弃。
5. **模型解析失败即报错。** 每个模型都真实扣积分，静默回落等于替用户乱花钱。

## 跨切面决策

- 零第三方依赖 —— `adr-0001`
- 控制台严格 CSP —— `adr-0002`
- 协议层抽出 —— `adr-0003`
- 有意推迟的重构 —— `adr-0004`
- 只剩积分通道 —— `adr-0005`

## 已知限制

- **限流只做单密钥维度。** 没有全局并发兜底，也没有按上游账号对齐「每账号 3 个并发会话」——
  后者能从根上消掉 `TM.00001041`，属于上游适配问题，尚未做。（`ratelimit`）
- **状态文件不支持多进程并发写。** `Save` 有「临时文件 + rename」的原子性，但没有
  读-改-写合并；两个进程共用一个状态文件会丢更新（不会写坏）。（`store`）
- **Windows 专属路径没有竞态检测。** CI 的 `-race` 只在 Linux job 跑，
  Windows job 只做 build + test；DPAPI 与定长回写路径未被 race detector 覆盖。
- **非 Windows 读不到 IDE 登录态。** 容器 / Linux 上只能手工粘贴 session JSON。（`codearts`）
