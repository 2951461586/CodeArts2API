# CodeArts2API

把本机 **CodeArts Agent**（华为云码道 / CodeArts Doer for Coding）桌面端的
上游模型服务，逆向复刻成一个 **OpenAI 兼容的 Go 网关**。

复用桌面端已登录的会话凭据，无需另申请密钥；纯 Go 标准库实现，单文件约 10 MB，
零外部依赖。

- 🎁 **免费福利 Token** —— 复用官方[限时领取 Tokens](https://support.huaweicloud.com/offers-codeartsagent/codeartsagent_offers_0001.html)活动，**每人每日 1000 万 Tokens**
- 💳 **积分通道** —— 需要时也可调用 `glm-5.3` / `GLM-5.2` 等订阅模型（按积分计费，控制台可一键关闭）
- 🔁 **每日自动签到** —— 内置定时领取免费福利 Token，IDE 没开 / 跨天 / 多账号都不漏
- 🔐 **三种凭据来源** —— 桌面端登录态导入 / 本地浏览器登录（OAuth 回调）/ 手工录入
- 🔌 **双协议** —— `/v1/chat/completions`（OpenAI）+ `/v1/messages`（Anthropic），含思维链与工具调用
- 🖥️ **内嵌 Web 控制台** —— 账号池 / 密钥 / 日志 / 额度进度 / 模型通道一目了然

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

**目录**：[一、快速开始](#一快速开始) ·
[二、模型与两条通道](#二支持的模型与两条通道) · [三、凭据与续期](#三凭据与续期) ·
[四、控制台](#四控制台) · [五、HTTP 接口](#五http-接口) ·
[六、工程结构](#六工程结构) · [七、安全说明](#七安全说明) ·
[八、免责声明](#八免责声明) · [九、许可证](#九许可证)

---

## 一、快速开始

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

## 二、支持的模型与两条通道

CodeArts 上游用请求头 **`maas_type`** 区分两条完全不同的通道 ——
同一个模型名在不同通道下是否可用、是否计费都不一样：

| 通道 | 请求头 | 计费 | 模型清单来源 |
| --- | --- | --- | --- |
| **免费福利 Token**（benefit） | `maas_type: benefit` | 消耗福利 Token（每日 1000 万，当日 24:00 清零） | `GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config` |
| **积分**（credit） | **不带** `maas_type` | 按积分计费，真实扣费 | `GET https://snap-access.cn-north-4.myhuaweicloud.com/v1/model/builtin` |

网关会按模型自动选通道，客户端**不需要关心**这件事。

### 2.1 免费福利 Token 活动

CodeArts 官方有一个**限时领取 Tokens** 活动：登录后客户端自动领取，
**每位用户每日 1000 万 Tokens**。

| 项目 | 说明 |
| --- | --- |
| 适用人群 | 所有用户 |
| 每日额度 | **1000 万 Tokens / 人 / 天** |
| 清零规则 | **当日 24:00 清零，不累积至次日** |
| 领取方式 | 登录成功后客户端**自动领取**（本网关复用同一份凭据，因此自动共享同一份额度） |
| 开始时间 | 码道 IDE / 插件：2026-08-19 22:00<br>桌面端 / 网页端：2026-09-15 00:00<br>小程序：2026-09-17 00:00（均为 UTC+8） |
| 结束时间 | 另行通知（官方保留随时终止或调整活动的权利） |
| 活动模型 | `deepseek-v4.1-flash`、`glm-5.3-flash`、`deepseek-v4-flash-0731`、`deepseek-v4-pro-0813` |

> 官方活动页：<https://support.huaweicloud.com/offers-codeartsagent/codeartsagent_offers_0001.html>
> （更新于 2026-09-23）

四个活动模型的规格：

| model_id | 上下文 | 最大输出 |
| --- | --- | --- |
| `glm-5.3-flash` | 1,048,576 | 131,072 |
| `deepseek-v4-flash-0731` | 1,048,576 | 393,216 |
| `deepseek-v4-pro-0813` | 1,000,000 | 393,216 |
| `deepseek-v4.1-flash` | 1,000,000 | 384,000 |

#### ⚠️ 福利 Token 与积分包**互不相通**

官方 FAQ 明确说明：这四个是**专属福利模型**，**仅消耗福利 Token**。
福利 Token 额度用完后，这四款模型将**不可再使用**，**也不能**用积分包继续跑 ——
只能改用积分通道的模型（`glm-5.3` / `GLM-5.2` 等）。

这也是网关把两条通道严格分开、并且**对无法识别的模型名默认落到免费通道**的原因：
宁可报错，也不静默扣积分。

#### ⚠️ 额度按账号共享

控制台「概览」里的进度条与 CodeArts Agent 用的是同一份配额。
网关跑太多会把 IDE 当天的额度吃光；额度每天 24:00 重置，不会累积。

#### 每日自动签到

网关内置**每日自动签到**，默认开启、每天 00:05 执行（可在控制台改时刻或关闭）。

官方活动页说明「登录成功后客户端会自动为你领取该福利」，所以 IDE 运行时其实
已经替我们领了。网关的自动签到是这三种情况的兜底：

- **IDE 没开** —— 网关独立跑的时候
- **跨天** —— 福利 Token 当日 24:00 清零，第二天需要重新领
- **多账号** —— IDE 只登录了一个账号

领取接口 `GET /api/v1/benefit/claim` 是**幂等**的：当天已领过再领一次仍返回
`0000 success`，额度不变，所以重复领取没有副作用。

> 实现上用「每 5 分钟轮询 + 按本地日期去重」，而不是算准 00:05 触发一次 ——
> 这样能避开休眠唤醒、时区切换、进程重启导致的漏签。
> 签到失败只记错误、不写日期，下一轮会自动重试。

控制台「账号」页有 **立即签到领福利** 按钮，概览页有 `今日签到 n/m` 计数。

#### ⚠️ 活动条款要点

- **不可转让**：权益不可转让，不得兑换现金、优惠券。
- **取消与收回**：若用户违反账号或服务相关的义务、条款、政策、法律或规则，
  华为云保留**立即取消、作废并收回**已发放权益的权利。
- **活动期限 / 变更**：官方保留随时终止或调整活动及其条款的权利，
  变更将通过更新活动页或其他通知形式告知。

### 2.2 积分通道（计费）

需要订阅 / 企业套餐，按上游返回的 `credit[].ratio_display` 倍率扣积分：

| model_id | 类别 | 积分倍率 |
| --- | --- | --- |
| `glm-5.3` | 文本生成 | 0.79x |
| `GLM-5.2` | 文本生成 | 0.7x |
| `openpangu-2.0-pro` | 文本生成 | 0.7x |
| `glm-5.2-sft-harmony` | 文本生成 | 0.7x |
| `openpangu-2.0-flash` | 文本生成 | 0.32x |
| `Qwen3-VL-235B` | 图片理解 | — |
| `kimi-k2.6-vl` | 图片理解 | — |

> ⚠️ **积分通道会真实扣费。** 控制台可一键关闭（设置 → 允许积分模型），
> 关闭后只放行免费额度通道的模型。
> 「探活」按钮永远使用免费通道的模型，不会因为点一下而扣积分。

#### 已知上游怪癖：`glm-5.3` 只支持流式

实测 `glm-5.3` 的**非流式**接口会返回一个 **HTTP 400 + 空 body**，
只有流式可用。网关检测到这种响应后会自动改用流式请求、在本地把分片聚合成
一次性响应，对客户端保持「非流式」语义 —— 并且会记住这个模型，下次直接走流式，
不再多打一次空枪。

```bash
# 这两条都能正常工作
curl ... -d '{"model":"glm-5.3","messages":[...]}'                # 网关内部自动聚合
curl ... -d '{"model":"glm-5.3","messages":[...],"stream":true}'  # 原生流式
```

#### 已知上游限制：每账号 3 个并发会话

打满时上游返回 `TM.00001041 并发会话数已达上限(3个)`。网关把它识别为
**临时性故障**：退避后重试、并且**不会**把账号标记为不可用 ——
否则一次并发打满就会让健康账号被熔断。

> 如果你要跑高并发，建议在控制台多导入几个账号，网关会自动轮询并在失败时换号。

### 2.3 模型别名

内置别名（可在控制台改成任意映射）：

```json
{"gpt-4o":"glm-5.3-flash","gpt-5":"deepseek-v4-pro-0813",
 "claude-sonnet-4":"deepseek-v4-pro-0813","deepseek-chat":"deepseek-v4-flash-0731"}
```

别名会继承目标模型的通道。**无法识别的模型名默认落到免费额度通道** ——
宁可报错，也不静默扣积分。

### 2.4 模型清单接口

`GET /v1/models` 会在 `codearts` 扩展字段里带上通道与计费信息：

```json
{"id":"glm-5.3","object":"model","owned_by":"codearts",
 "codearts":{"channel":"credit","credit":"0.79x","billable":true,
             "name":"glm-5.3","context":0,"max_output":0,
             "description":"面向复杂编程与长程任务"}}
```

---

## 三、凭据与续期

### 3.1 三种获取凭据的方式

| 方式 | 适用场景 | 说明 |
| --- | --- | --- |
| **自动导入桌面端登录态** | Windows，已装 CodeArts Agent 并登录过 | 启动时自动扫描 `%APPDATA%\codearts-agent`，无需任何操作 |
| **本地浏览器登录** | 没装 IDE，或想给网关单独登录一个账号 | 网关自己起 OAuth 回调服务器，用浏览器完成登录 |
| **手工录入 / session JSON** | 跨平台、Docker、远程部署 | 填 STS 临时 AK/SK，或粘贴解密后的 session JSON（含 refresh_token，仍可自动续期） |

#### 桌面端登录态是怎么解出来的

CodeArts Agent 是 VS Code fork，登录会话存在 VS Code 的 secret storage 里，
加密方式和 Chromium/Electron 的 OSCrypt 完全一致：

1. `Local State` 的 `os_crypt.encrypted_key` = `"DPAPI"` + `CryptProtectData(AES-256 主密钥)`
2. `state.vscdb` 的 `ItemTable` 里，`secret://{...HuaweiCloudSession}` 的值 =
   `"v10"` + nonce(12) + AES-256-GCM(明文‖tag)

网关自己实现了一个极简 SQLite 只读解析器（含溢出页拼装），**零第三方依赖**。
想手动确认能否读到凭据：

```bash
go run ./cmd/probe              # 全量诊断：凭据 / 模型 / 额度 / 签到 / 对话探活
go run ./cmd/probe -dump-keys   # 只列出 state.vscdb 里 ItemTable 的全部键名
```

### 3.2 本地浏览器登录（OAuth 回调）

控制台「账号」页点 **浏览器登录（本机回调）** 即可。网关做的事：

```text
1. 生成 PKCE 对（code_verifier = 32 字节 hex，challenge = S256）
   + DPoP P-256 密钥对
2. 在 127.0.0.1 上起一个随机端口（≥10000，与 IDE 取端口策略一致）的回调服务器
3. 给出授权地址（在浏览器里打开）：
     https://codearts.huaweicloud.com/portal/authorize
       ?theme=2&locale=zh-cn&uri_scheme=codearts-agent&client_id=codearts-agent
       &port=<回调端口>&code_challenge=<S256>&code_challenge_method=SHA-256
       &ticket_id=<hex>&plugin-name=snap_AIIDE&plugin-version=<ver>
4. 浏览器登录后回调两次（与 IDE 行为一致）：
     GET /oauth/callback?secret=<..>&redirect=<..>   → 307 跟随 redirect
     GET /oauth/callback?code=<authorizationCode>    → 拿到授权码
5. 用授权码换 STS 凭据：
     POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens
       DPoP: <ES256 JWS>
       client_id=codearts-agent&code=<code>&code_verifier=<verifier>
       &grant_type=authorization_code&redirect_uri=<callback>
6. 凭据与 DPoP 私钥 / PKCE verifier 一起落盘，
   之后凭 refresh_token 就能长期自动续期
```

> ⚠️ 回调地址是 `127.0.0.1`，所以**浏览器必须与网关在同一台机器上**。
> 容器 / 远程部署请改用「桌面端导入」或「session JSON 导入」。

### 3.3 续期策略

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

### 3.4 跨平台 / Docker

DPAPI 只有 Windows 有，所以在 Linux / macOS / Docker 里无法自动读取桌面端凭据。
此时有两种方式：

1. 控制台「手工录入」：填 STS 临时 AK / SK / Security Token
2. 控制台「从 session JSON 导入」：粘贴解密后的 session JSON（含 refresh_token 与 DPoP 私钥，可续期）

---

## 四、控制台

<http://127.0.0.1:10087/>，单文件、无外部依赖、可离线使用。

- **概览**：请求统计、今日免费额度进度条、今日签到 n/m、模型清单（含通道与积分倍率）、一键刷新模型
- **账号**：导入 / **浏览器登录** / 手工录入 / 从 session JSON 导入 / 探活 / **签到** / 重读 / 启停 / 删除，AK 与域 ID 脱敏展示，并显示「今日签到」状态
- **密钥**：签发 `sk-ca-...` 调用密钥（未签发时 `/v1/*` 允许无密钥调用）
- **日志**：只记元信息（模型、耗时、入出长度），不落对话内容
- **设置**：默认 / 强制模型、**允许积分模型开关**、**每日自动签到开关与时刻**、
  超时、重试、日志条数、CORS、续期开关、上游身份头、模型别名、管理密码

鉴权规则：设了管理密码就必须带 `X-Admin-Password`（或 `Authorization: Bearer`）；
没设密码则只允许本机环回访问。所有写操作额外做同源校验。

---

## 五、HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/health` | 探活 + 账号就绪数 |
| GET | `/v1/models` | 模型清单（OpenAI 格式，附带 `codearts` 扩展字段） |
| POST | `/v1/chat/completions` | OpenAI 兼容，支持流式 / 工具调用 / `reasoning_content` |
| POST | `/v1/messages` | Anthropic Messages API 兼容（含 `thinking` / `tool_use` 双向映射） |
| GET/POST/DELETE | `/admin/api/*` | 控制台 API（见下表） |

控制台 API：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/admin/api/state` | 首屏状态（账号 / 密钥 / 模型 / 设置 / 统计 / 额度 / 今日签到） |
| POST | `/admin/api/accounts` | 账号操作：`import` / `manual` / `session-json` / `remove` / `toggle` / `clear-errors` / `reimport` |
| GET/POST/DELETE | `/admin/api/keys` | API 密钥增删改查 |
| GET/DELETE | `/admin/api/logs` `/admin/api/stats` | 请求日志与统计 |
| GET/POST | `/admin/api/settings` | 运行设置 |
| POST | `/admin/api/checkin` | 账号连通性探活（永远走免费通道，不扣积分） |
| POST | `/admin/api/claim` | **每日签到领福利 Token**（全部账号或指定 `id`） |
| POST | `/admin/api/models/refresh` | 从上游刷新模型清单 |
| GET | `/admin/api/balance` | 查询免费额度余额 |
| POST | `/admin/api/login/start` | **启动浏览器登录**，返回授权 URL |
| POST | `/admin/api/login/poll` | 轮询登录状态，成功后自动入库 |
| POST | `/admin/api/login/cancel` | 取消浏览器登录 |

上游返回的 `reasoning_content`（思维链）会原样透传，
OpenAI 侧在 `choices[].message.reasoning_content` / `delta.reasoning_content`，
Anthropic 侧映射为 `thinking` 内容块。

---

## 六、工程结构

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
│  │  ├─ login.go              # 本地浏览器登录（OAuth PKCE + 回调服务器）
│  │  ├─ benefit.go            # 每日免费福利 Token 领取
│  │  ├─ sse.go                # SSE 聚合（流式→非流式兜底）+ 错误分类
│  │  ├─ manager.go            # 凭据解析与续期编排
│  │  └─ debug.go              # state.vscdb 键名转储
│  ├─ registry/          # 模型清单与别名解析
│  ├─ server/            # OpenAI / Anthropic 兼容层 + 控制台
│  │  ├─ chat.go  anthropic.go  models.go
│  │  ├─ claim.go              # 每日自动签到调度
│  │  ├─ login.go              # 浏览器登录 API
│  │  └─ admin.go  server.go  web/
│  └─ store/             # APIKey / 日志 / 设置 / 账号池
├─ tools/                # 逆向 PoC 脚本（Python）
└─ docs/RECON.md         # 逆向勘察报告
```

---

## 七、安全说明

- 状态文件（`codearts2api-data.json`）里含 STS 凭据与 `refresh_token`，
  已用 `0600` 权限写入；请勿提交到版本库（见 `.gitignore`）。
- 控制台对 AK / 域 ID / APIKey 一律脱敏展示，`Authorization` 与 `X-Security-token`
  从不写进日志。
- 网关默认只监听 `127.0.0.1`。要对外提供服务请自行加反向代理与鉴权，
  并显式配置 `CORS 允许来源`。
- 本项目仅供本机个人复用已授权的账号权益，请遵守华为云服务条款。

---

## 八、免责声明

- 本项目是**互操作性研究**的产物，目的是让已有的账号权益能在用户自己选择的
  客户端里使用，不提供、不代理、不转售任何模型能力。
- 本项目**不包含**任何华为云 / CodeArts Agent 的二进制、源码或资源文件；
  仓库里全部是独立编写的 Go 代码与一份基于公开可观察行为的分析笔记。
- 使用本项目意味着你自行承担全部风险：上游接口与鉴权方式随时可能变更，
  账号也可能因违反服务条款而被限制。作者不对由此产生的任何后果负责。
- 请遵守你所在地的法律法规以及华为云的服务条款。
  如果你不认同这一点，请不要使用本项目。

---

## 九、许可证

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
