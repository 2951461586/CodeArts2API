# 文档导航

| 文档 | 读它解决什么问题 | 主要读者 |
| --- | --- | --- |
| [../README.md](../README.md) | 项目是什么、怎么跑起来、有哪些接口 | 所有人 |
| [UPSTREAM.md](UPSTREAM.md) | **当前**上游协议事实：端点、身份头、通道、签名、错误码 | 改上游层的人 |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | 报错了怎么办（症状 → 原因 → 处置） | 使用者 / 运维 |
| [../SECURITY.md](../SECURITY.md) | 威胁模型、已实施防护、残余风险、加固清单 | 部署者 / 安全审阅 |
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

1. `README.md` 的「六、工程结构」
2. `UPSTREAM.md`（协议事实）
3. `RECON.md`（为什么这么实现、证据在哪）
4. `README.md` 的「七、开发与自检」
5. 动手前看一眼 [adr/](adr/)：那里记了已采纳的决策与**有意推迟**的重构，
   避免重复讨论或误改（例如「为什么控制台不用 SSE」「为什么不拆 codearts」）。

---

## 架构决策记录（ADR）

| ADR | 结论 |
| --- | --- |
| [0001](adr/0001-stdlib-only.md) | 只依赖 Go 标准库（签名 / SQLite / DPAPI 自研） |
| [0002](adr/0002-console-strict-csp.md) | 控制台拆三文件，CSP 去掉 `unsafe-inline` |
| [0003](adr/0003-protocol-layer.md) | 协议转换抽到 `internal/protocol`，先测试后搬家 |
| [0004](adr/0004-deferred-refactors.md) | 有意推迟的重构及原因（codearts 拆分、SSE、slog…） |

---

## 文档维护约定

- **`UPSTREAM.md` 是活文档**：上游行为变化时**先**改它，再改代码与测试。
- **`RECON.md` 是时间点快照**：记录当时的证据与推理，不随实现更新；
  两者冲突时以 `UPSTREAM.md` 与代码为准。
- 用户可见行为（接口、错误语义、默认值）变化时，同步 `README.md`。
- 每条「处置建议」都应能在代码或日志里找到对应依据，避免写下无法验证的说法。
