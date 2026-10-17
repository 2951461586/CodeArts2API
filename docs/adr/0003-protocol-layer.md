# ADR 0003：把 Anthropic ↔ OpenAI 转换抽成 `internal/protocol`

- 状态：已采纳
- 日期：2026-10

## 背景

`internal/server/anthropic.go` 原本 712 行，同时承担三件事：

1. HTTP 处理（读 body、鉴权、选模型、错误映射）；
2. 协议转换（Anthropic 请求 → OpenAI 请求；OpenAI 响应/SSE → Anthropic 响应/SSE）；
3. SSE 编码（`message_start` / `content_block_delta` / `message_stop`）。

其中第 2 类是**纯数据转换**，却被锁在一个依赖 `net/http`、`store`、`codearts`
的包里，只能通过构造整个 Server 来测试 —— 而实际上它此前**完全没有测试**。
转换出错不会报错，只会让客户端拿到语义错误的结果（内容块顺序、`stop_reason`、
`tool_use`/`tool_result` 配对）。

## 决策

- 新增 `internal/protocol`，只放类型与纯函数：
  `Request` / `Message` / `Tool` / `Reply`，
  `ToOpenAI` / `MessageToOpenAI` / `ToolChoice` / `SystemText` / `ImageURL` /
  `ToolResultText` / `FromOpenAI` / `EstimatePromptLen`。
- 该包**不 import `net/http`**，也不碰响应写出。
- `internal/server/anthropic.go` 只保留 HTTP 与 SSE（约 330 行）。

**执行顺序**：先写金标准测试（`internal/protocol/anthropic_test.go`）钉住现有行为，
再搬家。测试在搬家前后都通过，证明行为未变。

## 后果

### 好处

- 转换逻辑可脱离网络完整单测，覆盖内容块顺序、`stop_reason` 取值、
  工具调用配对、图片编码、空响应兜底等易错点；
- `server` 不再同时承担传输与协议适配，职责边界清晰；
- 将来加第三个协议（例如 Gemini）时，只需在 `protocol` 里加一个适配器。

### 代价

- 多一个包、多一层间接；简单改动可能要跨两个文件；
- `Reply.Payload` 仍是 `map[string]any`（未做成强类型）。这是有意的：
  上游是 OpenAI 兼容协议，字段集合会随上游变化，过早强类型化会变成维护负担。
  如果将来要生成 OpenAPI 或做严格校验，再考虑类型化。

### 回归防护

`internal/protocol/anthropic_test.go` 覆盖：

- system（字符串 / 文本块数组）、`max_tokens`、`temperature`、`stop`；
- `tool_use` → `tool_calls`、`tool_result` → `role=tool`；
- 图片 `base64` / `url` 两种来源；
- `tool_choice` 四态（`any`/`tool`/`none`/`auto`）与非法 JSON；
- 响应内容块顺序 `thinking → text → tool_use`；
- `finish_reason` → `stop_reason`（`stop`/`length`/`tool_calls`）；
- 空响应兜底（必须给一个空 text 块）。
