# 文档导航

| 文档 | 读它解决什么问题 | 主要读者 |
| --- | --- | --- |
| [../README.md](../README.md) | 项目是什么、怎么跑起来、有哪些接口 | 所有人 |
| [UPSTREAM.md](UPSTREAM.md) | **当前**上游协议事实：端点、身份头、通道、签名、错误码 | 改上游层的人 |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | 报错了怎么办（症状 → 原因 → 处置） | 使用者 / 运维 |
| [../SECURITY.md](../SECURITY.md) | 威胁模型、已实施防护、残余风险、加固清单 | 部署者 / 安全审阅 |
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

---

## 文档维护约定

- **`UPSTREAM.md` 是活文档**：上游行为变化时**先**改它，再改代码与测试。
- **`RECON.md` 是时间点快照**：记录当时的证据与推理，不随实现更新；
  两者冲突时以 `UPSTREAM.md` 与代码为准。
- 用户可见行为（接口、错误语义、默认值）变化时，同步 `README.md`。
- 每条「处置建议」都应能在代码或日志里找到对应依据，避免写下无法验证的说法。
