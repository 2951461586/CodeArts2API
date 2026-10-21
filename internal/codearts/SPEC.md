---
id: codearts
type: module-design
title: codearts —— 上游复刻层
status: draft
parent: architecture
references: [adr-0001, adr-0005]
---

# codearts

## 职责

复刻 CodeArts Agent 与华为云上游之间的**全部**交互：把凭据从桌面端安全读出来、
按上游要求签名、发请求、按需续期，以及领取运营活动奖励。

它是整个网关里唯一知道上游长什么样的地方。上游端点、身份头、签名算法、业务错误码
全部收敛在这里（端点常量集中在 `client.go` 顶部；协议事实见 `docs/UPSTREAM.md`）。

## 边界

- **不依赖任何内部包。** 可脱离网关单测。
- **不认识 HTTP 的请求/响应概念。** 只接受 `context` + `Session` + 字节，只返回字节与 `error`；
  状态码与 `error.type` 由 `apierr` 决定。
- **不决定重试策略。** 只负责把错误分类成「凭据失效 / 临时故障 / 空回复拒绝」，重试是 `server` 的事。

## 关键决策

- **上游会话 ID 按凭据复用，不做缓存。** 上游按 `x-ot-session-id` 统计并发会话（每账号 3 个），
  每个请求都换新 ID 会迅速打满上限并报 `TM.00001041`。`UpstreamSessionID` 因此是纯函数 ——
  `Session` 被多个 goroutine 共享，写缓存字段就是数据竞争。
- **SQLite 只做「只读 + 同长度原地覆写」。** `PatchItemValue` 拒绝改变长度的写入：
  不新增/删除 cell、不改页结构、不动 freelist，因此不存在把 IDE 数据库改坏的结构性风险。
- **续期是整体替换，不是原地修改。** 上层可以安全地共享 `*Session` 指针。
- **临时故障不能熔断账号。** 并发上限、429、5xx 只是「换个时机重试」，把它们计成账号故障
  会让健康账号被误标不可用。
- **空 body 的 4xx 值得用流式重试一次** —— 上游对只支持流式的模型就是这么拒绝非流式请求的。

## 已知限制

- **非 Windows 读不到 IDE 登录态。** `ReadSession` 在非 Windows 平台返回
  `ErrUnsupportedPlatform`；容器 / Linux 上只能手工粘贴 session JSON。
- **DPAPI 与定长回写路径没有 race detector 覆盖**（CI 的 `-race` 只在 Linux job 跑）。
