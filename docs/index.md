# 文档导航

| 文档 | 读它解决什么问题 | 主要读者 |
| --- | --- | --- |
| [../README.md](../README.md) | 项目是什么、怎么跑起来、有哪些接口 | 所有人 |
| [UPSTREAM.md](UPSTREAM.md) | **当前**上游协议事实：端点、身份头、通道、签名、错误码 | 改上游层的人 |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | 报错了怎么办（症状 → 原因 → 处置） | 使用者 / 运维 |
| [../SECURITY.md](../SECURITY.md) | 威胁模型、已实施防护、残余风险、加固清单 | 部署者 / 安全审阅 |
| [../goal-and-requirements.md](../goal-and-requirements.md) | 产品是什么、为什么、明确不做什么 | 所有人 |
| [../architecture.md](../architecture.md) | 模块边界、依赖边、不变量、已知限制 | 改代码的人 |
| `internal/*/SPEC.md` | 每个模块的职责与边界 | 改那个模块的人 |
| [adr/](adr/) | 关键架构决策（为什么这样写、为什么有些事没做） | 维护者 |
| [RECON.md](RECON.md) | 逆向过程与证据位置（**历史快照**，不代表现状） | 想理解「为什么这么实现」的人 |

---

## 按角色阅读

### 第一次用

1. `README.md` 的「一、快速开始」
2. 卡住了 → `TROUBLESHOOTING.md`

### 要部署到容器 / 服务器

1. `README.md` 的「3.5 跨平台 / Docker」
2. `SECURITY.md` 的「加固清单」
3. `TROUBLESHOOTING.md` 的「Docker 里打不开控制台」

### 要改代码

1. [`goal-and-requirements.md`](../goal-and-requirements.md) 与 [`architecture.md`](../architecture.md)
   （产品边界与模块不变量）
2. 要动哪个包，就读那个包的 `SPEC.md`
3. `README.md` 的「六、工程结构」与「七、开发与自检」
4. `UPSTREAM.md`（协议事实）—— 改上游层必读
5. 动手前看一眼 [adr/](adr/)：那里记了已采纳的决策与**有意推迟**的重构，
   避免重复讨论或误改（例如「为什么控制台不用 SSE」「为什么不拆 codearts」）。
6. `RECON.md`（为什么这么实现、证据在哪）—— 历史快照，不是现状

---

## 规格（spec graph）

规格是**这个项目的权威事实来源**：架构、决策、契约、边界，以及代码本身看不出来的意图。
它们带 `id` / `type` frontmatter，可以用 `spec_graph` / `spec_get` / `spec_grep` 检索。

```
goal                 goal-and-requirements.md        产品目标、能力、非目标、原则
└─ architecture      architecture.md                 模块依赖边、不变量、已知限制
   ├─ codearts       internal/codearts/SPEC.md       上游复刻层
   ├─ protocol       internal/protocol/SPEC.md       Anthropic ↔ OpenAI 纯转换
   ├─ apierr         internal/apierr/SPEC.md         错误 → 状态码/type 唯一映射
   ├─ store          internal/store/SPEC.md          状态持久化与账号池
   ├─ registry       internal/registry/SPEC.md       模型清单与别名
   ├─ ratelimit      internal/ratelimit/SPEC.md      按密钥的自我保护限流
   ├─ server         internal/server/SPEC.md         HTTP 层 + 控制台（组合根）
   │  └─ adr-0002    docs/adr/0002-…                  控制台三文件 + 严格 CSP
   ├─ adr-0001       docs/adr/0001-…                  只依赖标准库
   ├─ adr-0003       docs/adr/0003-…                  协议层抽出
   ├─ adr-0004       docs/adr/0004-…                  有意推迟的重构
   ├─ adr-0005       docs/adr/0005-…                  只剩积分通道
   └─ adr-0006       docs/adr/0006-…                  凭据形态显式建模（STS / 永久 AK·SK）
```

新建的 `goal` / `architecture` / 六个模块 `SPEC.md` 目前是 `status: draft` ——
审阅通过后改成 `active`。ADR 已是 `active`（它们本来就已采纳）。

`UPSTREAM.md`、`SECURITY.md`、`RECON.md` 与 `TROUBLESHOOTING.md` **不是**规格：
前两个是规格的输入，`RECON.md` 是历史快照，`TROUBLESHOOTING.md` 是操作手册。

---

## 架构决策记录（ADR）

| ADR | 结论 |
| --- | --- |
| [0001](adr/0001-stdlib-only.md) | 只依赖 Go 标准库（签名 / SQLite / DPAPI 自研） |
| [0002](adr/0002-console-strict-csp.md) | 控制台拆三文件，CSP 去掉 `unsafe-inline` |
| [0003](adr/0003-protocol-layer.md) | 协议转换抽到 `internal/protocol`，先测试后搬家 |
| [0004](adr/0004-deferred-refactors.md) | 有意推迟的重构及原因（codearts 拆分、SSE、slog…） |
| [0005](adr/0005-remove-benefit-channel.md) | 删除免费权益（benefit）通道，只保留积分通道 |
| [0006](adr/0006-credential-kind.md) | 凭据形态显式建模：STS 临时凭据 / IAM 永久 AK·SK（不靠过期时间猜） |

---

## 文档维护约定

- **规格优先**：改动跨过模块边界、推翻某条决策、或让某条「已知限制」失效时，
  **同一次改动**里更新对应 `SPEC.md` / `architecture.md` / `goal-and-requirements.md`。
  规格一旦和代码脱节就不再是事实来源。
- **`UPSTREAM.md` 是活文档**：上游行为变化时**先**改它，再改代码与测试。
- **`RECON.md` 是时间点快照**：记录当时的证据与推理，不随实现更新；
  两者冲突时以 `UPSTREAM.md` 与代码为准。
- 用户可见行为（接口、错误语义、默认值）变化时，同步 `README.md`。
- 每条「处置建议」都应能在代码或日志里找到对应依据，避免写下无法验证的说法。
