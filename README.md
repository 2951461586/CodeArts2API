# CodeArts2API

把本机 **CodeArts Agent**（华为云码道 / CodeArts Doer for Coding）桌面端的
上游模型服务，逆向复刻成一个 **OpenAI 兼容的 Go 网关**。

复用桌面端已登录的会话凭据，无需另申请密钥；纯 Go 标准库实现，单文件约 10 MB，
零外部依赖。

```text
┌──────────────┐   /v1/chat/completions   ┌───────────────┐  SDK-HMAC-SHA256 签名   ┌───────────────────────────────────┐
│ 客户端        │   /v1/messages           │ CodeArts2API  │  + X-Security-token     │ snap-access.cn-north-4.myhuawei   │
│ OpenAI SDK   │   /v1/models             │   (本网关)     │ ──────────────────────▶ │ cloud.com/api/v2/chat/completions │
│ Anthropic SDK│ ───────────────────────▶ │               │ ◀────────────────────── │                                   │
│ NewAPI / pi  │   SSE 流式 / 非流式        │               │      OpenAI SSE         └───────────────────────────────────┘
└──────────────┘                          └───────┬───────┘
                                                  │ 只读解密（过期时可自行续期）
                                                  ▼
                       %APPDATA%\codearts-agent\Local State                 ← DPAPI 保护的主密钥
                       %APPDATA%\codearts-agent\User\globalStorage\state.vscdb
                          secret://{"extensionId":"huaweicloud.authentication",
                                    "key":"HuaweiCloudSession"}              ← AK / SK / STS / refresh_token
```

> 逆向过程与全部证据见 [`docs/RECON.md`](docs/RECON.md)。

**目录**：[一、与 Doubao2API 的差别](#一和-doubao2api-的差别) ·
[二、快速开始](#二快速开始) · [三、支持的模型](#三支持的模型) ·
[四、凭据与续期](#四凭据与续期) · [五、控制台](#五控制台) ·
[六、HTTP 接口](#六http-接口) · [七、工程结构](#七工程结构) ·
[八、安全说明](#八安全说明) · [九、免责声明](#九免责声明) · [十、许可证](#十许可证)

---

## 一、和 Doubao2API 的差别

| 维度 | Doubao2API | CodeArts2API |
| --- | --- | --- |
| 上游协议 | 私有 SSE（`content_block` 补丁流），需还原 | **OpenAI 原生，零协议转换** |
| 上游鉴权 | Cookie + CSRF 头 | **华为云 SDK-HMAC-SHA256 + `X-Security-token`** |
| 凭据有效期 | 长（Cookie） | **约 2 小时（STS 临时凭证），需要续期** |
| 凭据加密 | DPAPI + AES-256-GCM | **同源方案（VS Code 的 secret storage 就是 OSCrypt）** |

---

## 二、快速开始

```bash
go build -o codearts2api.exe .
./codearts2api.exe                 # 默认监听 127.0.0.1:10087
```

启动时会自动扫描 `%APPDATA%\codearts-agent` 并导入登录态：

```text
状态文件: F:\...\codearts2api-data.json
已导入账号 hid_xxxxxxxxxxxx（目录=...\codearts-agent，AK=HSTA**...**，有效期至 2026-01-01 13:13:08）
CodeArts2API dev 已启动: http://127.0.0.1:10087/
```

打开 <http://127.0.0.1:10087/> 进入控制台。

### 客户端接入

```bash
# OpenAI 兼容
curl http://127.0.0.1:10087/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","messages":[{"role":"user","content":"你好"}]}'

# Anthropic 兼容（pi / Claude Code 等）
curl http://127.0.0.1:10087/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.3-flash","max_tokens":256,"messages":[{"role":"user","content":"你好"}]}'
```

任何 OpenAI / Anthropic SDK 把 `base_url` 指到 `http://127.0.0.1:10087/v1` 即可。

### 命令行参数

| 参数 | 环境变量 | 说明 |
| --- | --- | --- |
| `-host` | `CODEARTS_HOST` | 监听地址（默认 `127.0.0.1`） |
| `-port` | `CODEARTS_PORT` | 监听端口（默认 `10087`） |
| `-data` | `CODEARTS_DATA_PATH` | 状态文件路径 |
| `-user-data` | `CODEARTS_USER_DATA` | CodeArts Agent 的 User Data 目录 |
| `-admin-password` | `CODEARTS_ADMIN_PASSWORD` | 管理密码 |
| `-import` | — | 只导入一次凭据并退出 |
| `-no-import` | — | 启动时不自动导入 |
| `-no-refresh` | — | 禁止网关自行续期（只读 IDE 会话） |

---

## 三、支持的模型

模型清单由上游 `GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config`
实时下发（网关每 10 分钟或点控制台按钮刷新）。**benefit 免费权益通道**当前提供：

| model_id | 上下文 | 最大输出 |
| --- | --- | --- |
| `glm-5.3-flash` | 1,048,576 | 131,072 |
| `deepseek-v4-flash-0731` | 1,048,576 | 393,216 |
| `deepseek-v4-pro-0813` | 1,000,000 | 393,216 |
| `deepseek-v4.1-flash` | 1,000,000 | 384,000 |

另外还会列出订阅 / 企业**内置模型**（`GLM-5.2`、`openpangu-2.0-pro` 等），
它们走另一条通道，需要企业套餐。

内置别名（可在控制台改成任意映射）：

```json
{"gpt-4o":"glm-5.3-flash","gpt-5":"deepseek-v4-pro-0813",
 "claude-sonnet-4":"deepseek-v4-pro-0813","deepseek-chat":"deepseek-v4-flash-0731"}
```

> ⚠️ 额度是**按账号共享**的：控制台显示的「今日已用 / 上限」与 CodeArts Agent
> 用的是同一份配额（默认 1000 万 tokens/天）。网关跑太多会把 IDE 当天的额度吃光。

---

## 四、凭据与续期

### 4.1 凭据从哪来

CodeArts Agent 是 VS Code fork，登录会话存在 VS Code 的 secret storage 里，
加密方式和 Chromium/Electron 的 OSCrypt 完全一致：

1. `Local State` 的 `os_crypt.encrypted_key` = `"DPAPI"` + `CryptProtectData(AES-256 主密钥)`
2. `state.vscdb` 的 `ItemTable` 里，`secret://{...HuaweiCloudSession}` 的值 =
   `"v10"` + nonce(12) + AES-256-GCM(明文‖tag)

网关自己实现了一个极简 SQLite 只读解析器（含溢出页拼装），**零第三方依赖**。
想手动确认能否读到凭据：

```bash
go run ./cmd/probe              # 全量诊断：凭据 / 模型 / 额度 / 对话探活
go run ./cmd/probe -dump-keys   # 只列出 state.vscdb 里 ItemTable 的全部键名
```

### 4.2 续期策略

STS 临时凭证只有约 2 小时有效期。网关按下面的优先级解析：

1. **`state.vscdb` 里的实时会话**（默认 10 秒缓存重读）—— IDE 运行时它自己会续期，永远最新
2. **网关本地缓存的会话** —— IDE 关闭后仍可继续用
3. **自行续期** —— 用 `refresh_token` + DPoP(ES256 JWS) + PKCE verifier
   打 `POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens`

自行续期默认开启，可在控制台关掉（`-no-refresh` 也行）。

> ⚠️ **refresh_token 可能是单次轮转的**。如果网关自己续期而不回写，
> IDE 下次启动时手里的 refresh_token 可能已失效，需要重新登录一次。
> 因此网关默认会把续期结果**回写 `state.vscdb`**，让 IDE 直接复用。

回写是**安全的原地覆写**：

- 只在「新密文长度不超过旧密文」时执行（不足处用尾随空格补齐，JSON 允许尾随空白）
- 不新增 / 删除 cell、不改页结构、不动 freelist，**不会触发页分裂或数据库重整**
- 写入前一定先备份一份 `state.vscdb.codearts2api.bak`

长度超出时按设计跳过（返回 `ErrWriteBackSkipped`），网关仍用自己保存的凭据正常工作。

### 4.3 跨平台 / Docker

DPAPI 只有 Windows 有，所以在 Linux / macOS / Docker 里无法自动读取桌面端凭据。
此时有两种方式：

1. 控制台「手工录入」：填 STS 临时 AK / SK / Security Token
2. 控制台「从 session JSON 导入」：粘贴解密后的 session JSON（含 refresh_token 与 DPoP 私钥，可续期）

---

## 五、控制台

<http://127.0.0.1:10087/>，单文件、无外部依赖、可离线使用。

- **概览**：请求统计、今日额度进度条、模型清单、一键刷新模型
- **账号**：导入 / 手工录入 / 探活 / 重读 / 启停 / 删除，AK 与域 ID 脱敏展示
- **密钥**：签发 `sk-ca-...` 调用密钥（未签发时 `/v1/*` 允许无密钥调用）
- **日志**：只记元信息（模型、耗时、入出长度），不落对话内容
- **设置**：默认 / 强制模型、超时、重试、日志条数、CORS、续期开关、
  上游身份头、模型别名、管理密码

鉴权规则：设了管理密码就必须带 `X-Admin-Password`（或 `Authorization: Bearer`）；
没设密码则只允许本机环回访问。所有写操作额外做同源校验。

---

## 六、HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/health` | 探活 + 账号就绪数 |
| GET | `/v1/models` | 模型清单（OpenAI 格式，附带 `codearts` 扩展字段） |
| POST | `/v1/chat/completions` | OpenAI 兼容，支持流式 / 工具调用 / `reasoning_content` |
| POST | `/v1/messages` | Anthropic Messages API 兼容（含 `thinking` / `tool_use` 双向映射） |
| GET/POST/DELETE | `/admin/api/*` | 控制台 API（`state` / `accounts` / `keys` / `logs` / `stats` / `settings` / `checkin` / `models/refresh` / `balance`） |

上游返回的 `reasoning_content`（思维链）会原样透传，
OpenAI 侧在 `choices[].message.reasoning_content` / `delta.reasoning_content`，
Anthropic 侧映射为 `thinking` 内容块。

---

## 七、工程结构

```text
CodeArts2API/
├─ main.go
├─ cmd/probe/            # 本机诊断工具
├─ internal/
│  ├─ codearts/          # 上游复刻层
│  │  ├─ session.go            # Session / Account 模型
│  │  ├─ session_windows.go    # DPAPI + AES-GCM 读取与安全回写
│  │  ├─ session_other.go      # 非 Windows 降级
│  │  ├─ sqlite.go             # 极简 SQLite 只读 + 定长原地写
│  │  ├─ signer.go             # 华为云 SDK-HMAC-SHA256
│  │  ├─ client.go             # inferhub 转发 / 模型 / 额度
│  │  ├─ refresh.go            # DPoP(ES256 JWS) 续期
│  │  ├─ manager.go            # 凭据解析与续期编排
│  │  └─ debug.go              # state.vscdb 键名转储
│  ├─ registry/          # 模型清单与别名解析
│  ├─ server/            # OpenAI / Anthropic 兼容层 + 控制台
│  └─ store/             # APIKey / 日志 / 设置 / 账号池
├─ tools/                # 逆向 PoC 脚本（Python）
└─ docs/RECON.md         # 逆向勘察报告
```

---

## 八、安全说明

- 状态文件（`codearts2api-data.json`）里含 STS 凭据与 `refresh_token`，
  已用 `0600` 权限写入；请勿提交到版本库（见 `.gitignore`）。
- 控制台对 AK / 域 ID / APIKey 一律脱敏展示，`Authorization` 与 `X-Security-token`
  从不写进日志。
- 网关默认只监听 `127.0.0.1`。要对外提供服务请自行加反向代理与鉴权，
  并显式配置 `CORS 允许来源`。
- 本项目仅供本机个人复用已授权的账号权益，请遵守华为云服务条款。

---

## 九、免责声明

- 本项目是**互操作性研究**的产物，目的是让已有的账号权益能在用户自己选择的
  客户端里使用，不提供、不代理、不转售任何模型能力。
- 本项目**不包含**任何华为云 / CodeArts Agent 的二进制、源码或资源文件；
  仓库里全部是独立编写的 Go 代码与一份基于公开可观察行为的分析笔记。
- 使用本项目意味着你自行承担全部风险：上游接口与鉴权方式随时可能变更，
  账号也可能因违反服务条款而被限制。作者不对由此产生的任何后果负责。
- 请遵守你所在地的法律法规以及华为云的服务条款。
  如果你不认同这一点，请不要使用本项目。

---

## 十、许可证

本项目以 [Apache License 2.0](LICENSE) 授权发布。

```text
Copyright 2026 Lizi

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
```

许可证覆盖的是**本项目自身的代码**。逆向过程中接触到的第三方内容
（CodeArts Agent 客户端二进制、`app.asar` 内容、华为云接口定义）
不包含在本仓库内，其权利仍归原权利人。

> 许可与使用限制是两件事：Apache-2.0 授予你使用、修改、分发本代码的权利，
> 但并不豁免你对上游服务条款的义务。
