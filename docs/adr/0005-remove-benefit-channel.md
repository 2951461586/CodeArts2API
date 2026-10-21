---
id: adr-0005
type: architecture-design
title: "ADR 0005：删除免费权益（benefit）通道，只保留积分通道"
status: active
parent: architecture
---

# ADR 0005：删除免费权益（benefit）通道，只保留积分通道

- 状态：已采纳
- 日期：2026-10

## 背景

网关原本同时支持 CodeArts 上游的两条通道：

| 通道 | 请求头 | 计费 |
| --- | --- | --- |
| benefit | `maas_type: benefit` | 消耗每日免费福利 Token（1000 万/日） |
| credit | 不带 `maas_type` | 按积分真实扣费 |

2026-10 官方「限时领取 Tokens」活动结束后，上游把整条 benefit 通道关掉了。
实测（同一份凭据、同一台机器）：

```text
GET /v1/benefit-gateway-config    -> 200 {"enabled":false,"is_examination_domain":false}
GET /api/v1/gateway/config        -> 200 {"error_code":"4000","error_msg":"invalid channel, group not found"}
GET /api/v1/benefit/claim         -> 200 {"error_code":"4000","error_msg":"invalid channel, group not found"}
GET /api/v1/user/tokens/balance   -> 200 {"error_code":"4000","error_msg":"invalid channel, group not found"}
GET /v1/model/builtin             -> 200（8 个模型，全部 enable:true）
```

于是网关进入一种很糟的状态：

1. `RefreshModels` 先拉 benefit 清单，失败就整体返回错误 —— 模型清单永远刷新不了，
   `/v1/models` 长期停在已失效的兜底清单上（`glm-5.3-flash` 等）；
2. 因为清单失效，`Resolve` 把大多数模型名回落到 benefit 通道 → 请求必然 4005/失败；
3. 控制台的「今日额度」进度条与「签到」按钮每次都报 `invalid channel, group not found`，
   用户看到的是红色报错而不是「活动结束了」。

## 决策

**彻底删除 benefit 通道**（不做「保留探测、活动恢复自动接回」）：

- 删 `internal/codearts/benefit.go`（领取、通道开关、错误翻译）与
  `FetchGatewayConfig` / `FetchBalance` / `GatewayConfig` / `TokensBalance`；
- 删 `Channel` 抽象与 `maas_type` 请求头 —— 只剩一条通道，就没有「选通道」这回事；
- 模型清单只来自 `GET /v1/model/builtin`；兜底清单换成当前 8 个积分模型；
- 删控制台的额度进度条、免费福利 Token 签到、`/admin/api/balance`；
- `/admin/api/claim` 收窄为「领取运营活动福利」（`/v1/ops/*` 仍然可用，与 benefit 无关）；
- `allow_credit_models` 保留，但语义从「允许积分模型」变成「允许真实扣费」的总开关；
- **未知模型名直接 404**，不再回落到任何模型。

## 后果

### 好处

- 没有「永远失败」的入口：模型清单、额度、签到三处假象一起消失；
- 模型清单有了唯一真相来源，刷新失败只影响新鲜度，不会污染清单；
- 计费安全：现在每个模型都真实扣费，静默回落等于替用户乱花钱，
  所以宁可直接 404 并在错误信息里列出可用模型；
- 少了一层「同一个模型名在不同通道下行为不同」的隐式语义，
  `registry` / `chat.go` / `anthropic.go` 都短了一截。

### 代价

- **活动若恢复，需要重新实现**（领取、额度、benefit 清单、`maas_type`）。
  这是用户明确选择的取舍：与其留一套永远失败的代码，不如删干净、按需重写。
- 老状态文件里的 `default_model` / `force_model` 可能还是已下线的模型名，
  启动时自动迁移到 `glm-5.3`（`store.applyDefaults`）。
- 内置别名（`gpt-4o` 等）的目标模型全部换成了现存模型，
  依赖旧映射（例如期望 `gpt-4o` 落到 `glm-5.3-flash`）的用户会看到行为变化。
- 若上游只是**按账号**关闭了 benefit 通道，其他账号恢复活动后本网关也不会使用它。
  实测的 `enabled:false` 无法区分这两种情况，按全局下线处理。

### 不做的事

- 不删除 `allow_credit_models`（现在是唯一的计费闸门）；
- 不重写 `welfare.go`（运营活动与 benefit 通道无关，仍可用）；
- 不保留任何 `maas_type` 分支做「未来兼容」。
