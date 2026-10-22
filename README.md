# CodeArts2API

把本机 **CodeArts Agent**（华为云码道 / CodeArts Doer for Coding）桌面端的
上游模型服务，逆向复刻成一个 **OpenAI 兼容的 Go 网关**。

复用桌面端已登录的会话凭据，无需另申请密钥；纯 Go 标准库实现，单文件约 10 MB，
零外部依赖。

- 💳 **积分通道（唯一通道）** —— 免费权益通道已于 2026-10 下线，全部模型按积分计费（`glm-5.3` / `GLM-5.2` / `deepseek-v4.1-flash` …），控制台可一键停用
- 🔁 **每日自动领取活动福利** —— 定时领取运营活动奖励（如「每日签到领 1000 积分」），IDE 没开 / 跨天 / 多账号都不漏
- 🔐 **三种凭据来源** —— 桌面端登录态导入 / 本地浏览器登录（OAuth 回调）/ 手工录入
- 🔌 **双协议** —— `/v1/chat/completions`（OpenAI）+ `/v1/messages`（Anthropic），含思维链与工具调用
- 🖥️ **内嵌 Web 控制台** —— 账号池 / 密钥 / 日志 / 模型清单与积分倍率一目了然

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

> 逆向过程与全部证据见 [`docs/RECON.md`](docs/RECON.md)（历史快照）。
> **当前**上游协议事实见 [`docs/UPSTREAM.md`](docs/UPSTREAM.md)，
> 报错处置见 [`docs/TROUBLESHOOTING.md`](docs/TROUBLESHOOTING.md)，
> 安全模型见 [`SECURITY.md`](SECURITY.md)，文档总览见 [`docs/index.md`](docs/index.md)。

**目录**：[一、快速开始](#一快速开始) ·
[二、支持的模型与计费](#二支持的模型与计费) · [三、凭据与续期](#三凭据与续期) ·
[四、控制台](#四控制台) · [五、HTTP 接口](#五http-接口) ·
[六、工程结构](#六工程结构) · [七、开发与自检](#七开发与自检) ·
[八、安全说明](#八安全说明) · [九、免责声明](#九免责声明) · [十、许可证](#十许可证)

**其它文档**：[文档导航](docs/index.md) · [产品目标](goal-and-requirements.md) ·
[架构与不变量](architecture.md) · [上游协议](docs/UPSTREAM.md) ·
[故障排查](docs/TROUBLESHOOTING.md) · [安全策略](SECURITY.md) · [逆向勘察](docs/RECON.md)

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
| `-allow-anonymous` | `CODEARTS_ALLOW_ANONYMOUS` | 未签发密钥时允许**非本机**匿名调用 `/v1/*`（默认仅本机直连） |
| `-import` | — | 只导入一次凭据并退出 |
| `-export-session` | — | 把本机登录态导出为 session JSON（路径，或 `-` 输出到 stdout）后退出 |
| `-no-import` | — | 启动时不自动导入 |
| `-no-refresh` | — | 禁止网关自行续期（只读 IDE 会话） |

---

## 二、支持的模型与计费

2026-10 起，CodeArts 上游**只剩积分（计费）通道**：请求不再携带 `maas_type`，
所有模型都按 `credit[].ratio_display` 倍率**真实扣积分**。模型清单只有一个来源：

```text
GET https://snap-access.cn-north-4.myhuaweicloud.com/v1/model/builtin
    Header: Agent-Type: PromptCenter
```

### 2.1 免费权益通道已下线（2026-10）

官方「限时领取 Tokens」活动（每人每日 1000 万 Tokens）结束后，上游把整条
benefit 通道一并关掉了。实测结果：

| 端点 | 结果 |
| --- | --- |
| `GET /v1/benefit-gateway-config` | `{"enabled":false,...}` |
| `GET /api/v1/gateway/config`（福利模型清单） | `4000: invalid channel, group not found` |
| `GET /api/v1/benefit/claim`（领福利 Token） | `4000: invalid channel, group not found` |
| `GET /api/v1/user/tokens/balance`（每日额度） | `4000: invalid channel, group not found` |

因此网关做了对应清理（决策与代价见 [ADR 0005](docs/adr/0005-remove-benefit-channel.md)）：

- **删除** benefit 通道的全部代码与 UI：福利模型清单、额度进度条、免费福利 Token 签到
- 请求头不再发送 `maas_type`
- 内置兜底清单换成当前 8 个积分模型
- **无法识别的模型名直接返回 `404`**，不再回落到任何模型 —— 现在每个模型都真实扣费，
  猜错模型等于替用户乱花钱
- 控制台的「活动福利」照旧可用（那是运营活动子系统，与 benefit 通道无关）

> 想恢复免费额度只能等官方重新开放活动。网关不会再静默尝试 benefit 通道，
> 也不会把「通道未开放」伪装成成功。

### 2.2 活动福利（积分）

CodeArts 的「活动福利」是一套运营活动子系统（IDE 左下角入口）。
实测（2026-10）线上共有三个 campaign：

| campaignId | 活动 | 类型 | 奖励 | 触发事件 | 能否自动领取 |
| --- | --- | --- | --- | --- | --- |
| 1 | **每日签到领1000积分** | `USER_LOGIN` | 1000 积分 | `user.login` | ✅ **可以** |
| 2 | 学生认证领4000积分 | `STUDENT_CERTIFIED` | 4000 积分 | `student.certified` | ❌ 需真实学生认证 |
| 3 | 推荐码上有礼 | `INVITE_USER` | 1000 积分/人 | `invite.success` | ❌ 需真实好友注册 |

接口：

```text
GET  /v1/ops/delivery?channel=IDE    列出活动与状态（claimable / status / pendingCount）
POST /v1/ops/claim                   {campaignId, idempotentKey, channel:"IDE"}
POST /v1/ops/invite/generate         {campaignId} 生成专属邀请码与邀请链接
```

**自动领取的边界**：网关只领**服务端明确标记 `claimable: true` 且靠时间推进**的活动。

实测服务端对不能直接领的活动会明确拒绝：

```jsonc
// 每日签到：今天已领，等次日刷新
{"code":40001,"message":"尚未到达权益刷新时间，暂时无法领取"}

// 邀请活动：必须由真实事件触发
{"code":40001,"message":"该活动类型需要先通过触发接口激活，无法直接领取"}
```

> ⚠️ **邀请奖励不会自动刷。** 邀请奖励必须由**真实好友通过你的专属链接完成注册**
> 才会产生；学生认证奖励同理。自动刷邀请属于多账号恶意注册，
> 活动条款明确禁止且涉嫌违法 —— 网关在代码层面就把 `INVITE_USER` /
> `STUDENT_CERTIFIED` 排除在自动领取之外（有单元测试锁定这个不变量）。
>
> 网关能帮你做的是**生成并展示你的邀请链接**：控制台「概览 → 生成我的邀请链接」，
> 或 `POST /admin/api/invite`。
>
> 💡 注意参与条件：如果你用的是 **Tokens 制套餐**，需要先免费变更为**积分制套餐**
> 才能参与活动（IDE 侧对应「活动福利」入口的提示）。

### 2.3 可用模型（积分通道）

需要订阅 / 企业套餐，按上游返回的 `credit[].ratio_display` 倍率扣积分。
以下是 2026-10 实测的完整清单（`/v1/models` 与之一致）：

| model_id | 名称 | 类别 | 积分倍率 |
| --- | --- | --- | --- |
| `glm-5.3` | GLM-5.3 | 文本生成 | 0.79x |
| `GLM-5.2` | GLM-5.2 | 文本生成 | 0.7x |
| `glm-5.2-sft-harmony` | GLM-5.2-ArkTS-SPARK | 文本生成 | 0.7x |
| `openpangu-2.0-pro` | OpenPangu-2.0-Pro | 文本生成 | 0.7x |
| `openpangu-2.0-flash` | OpenPangu-2.0-Flash | 文本生成 | 0.32x |
| `deepseek-v4.1-flash` | DeepSeek-V4.1-Flash | 文本生成 | 0.18x |
| `Qwen3-VL-235B` | Qwen3-VL-235B | 图片理解 | — |
| `kimi-k2.6-vl` | Kimi-K2.6-VL | 图片理解 | — |

> 图片理解类模型上游没有公布计费档，`credit` 字段为空（控制台显示「—」）。
> 上游也不返回上下文长度，因此 `context` / `max_output` 都是 0。

> ⚠️ **所有模型都会真实扣费。** 控制台的「允许真实消耗积分」开关关闭后，
> `/v1/*` 的模型调用一律返回 `403`（只在临时停服 / 排障时用）。
> 「探活」按钮会用默认模型发一次 `max_tokens=8` 的请求，消耗极少但**不为零**。

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

### 2.4 模型别名

内置别名（可在控制台改成任意映射）：

```json
{"gpt-4o":"glm-5.3","gpt-5":"GLM-5.2",
 "claude-sonnet-4":"GLM-5.2","deepseek-chat":"deepseek-v4.1-flash"}
```

完整映射：通用档（`gpt-4o` / `gpt-4` / `glm-4` / `codearts` …）→ `glm-5.3`，
旗舰档（`gpt-5` / `claude-3-5-sonnet` / `claude-sonnet-4` / `deepseek-reasoner` …）→ `GLM-5.2`，
`deepseek-chat` → `deepseek-v4.1-flash`。

规则：

- 解析顺序：精确 ID → 自定义别名 → 内置别名 → 大小写无关匹配；**没有模糊匹配**
- 全部失败 → `404`，错误信息里列出当前可用模型
- 自定义别名的目标模型不在当前清单里时，该别名既不生效也不出现在 `/v1/models` 里

### 2.5 模型清单接口

`GET /v1/models` 会在 `codearts` 扩展字段里带上计费信息：

```json
{"id":"glm-5.3","object":"model","owned_by":"codearts",
 "codearts":{"channel":"credit","credit":"0.79x","billable":true,
             "name":"GLM-5.3","context":0,"max_output":0,
             "description":"【限时体验】面向复杂编程与长程任务"}}
```

`channel` 恒为 `credit`（免费权益通道已下线），保留该字段只是为了兼容既有客户端。

---

## 三、凭据与续期

### 3.1 三种获取凭据的方式

| 方式 | 适用场景 | 说明 |
| --- | --- | --- |
| **自动导入桌面端登录态** | Windows，已装 CodeArts Agent 并登录过 | 启动时自动扫描 `%APPDATA%\codearts-agent`，无需任何操作 |
| **本地浏览器登录** | 没装 IDE，或想给网关单独登录一个账号 | 网关自己起 OAuth 回调服务器，用浏览器完成登录 |
| **手工录入 / session JSON** | 跨平台、Docker、远程部署 | 填 STS 临时 AK/SK（约 2 小时）或 **IAM 永久 AK/SK**（不过期、不续期），或粘贴解密后的 session JSON（含 refresh_token，仍可自动续期） |

#### 两种凭据形态

| 形态 | 生命周期 | 续期 | 签名 |
| --- | --- | --- | --- |
| **STS 临时凭据**（默认） | 约 2 小时 | refresh_token + DPoP + PKCE，可自动续期 | SDK-HMAC-SHA256 + `x-security-token` |
| **IAM 永久 AK/SK** | 不过期 | 无续期概念 | 纯 SDK-HMAC-SHA256（不带 security token） |

两者在网关内部由 `Session.CredentialKind` **显式区分**，不靠「有没有过期时间」猜：
永久凭据的 `expires_at` 本来就是零值，而早期实现把零值一律判为不可用，于是录入的
IAM AK/SK 在 110 分钟后永久失效，且 `HasRefresh` 为 false、Manager 也救不回来。
控制台对永久凭据显示「永不过期 / IAM AK/SK」，不会出现 1970 时间戳。

导出的 session JSON 是**可移植**的 —— 它带着 `refresh_token`、DPoP 私钥与 PKCE
verifier，所以在另一台机器 / 容器里导入后**仍然能自动续期**，不是只能活 2 小时。

#### 桌面端登录态是怎么解出来的

CodeArts Agent 是 VS Code fork，登录会话存在 VS Code 的 secret storage 里，
加密方式和 Chromium/Electron 的 OSCrypt 完全一致：

1. `Local State` 的 `os_crypt.encrypted_key` = `"DPAPI"` + `CryptProtectData(AES-256 主密钥)`
2. `state.vscdb` 的 `ItemTable` 里，`secret://{...HuaweiCloudSession}` 的值 =
   `"v10"` + nonce(12) + AES-256-GCM(明文‖tag)

网关自己实现了一个极简 SQLite 只读解析器（含溢出页拼装），**零第三方依赖**。
想手动确认能否读到凭据：

```bash
go run ./cmd/probe              # 全量诊断：凭据 / 模型清单 / 活动福利（不消耗积分）
go run ./cmd/probe -dump-keys   # 只列出 state.vscdb 里 ItemTable 的全部键名
```

### 3.2 导出 session JSON

三种方式任选：

```bash
# 1) 命令行，写文件
codearts2api.exe -export-session D:\codearts-session.json

# 2) 命令行，直接输出（方便管道）
codearts2api.exe -export-session - > codearts-session.json

# 3) 控制台：账号页 → 该账号的「导出」按钮（弹窗里可一键复制）
```

导出内容就是 IDE 存的那份 session，字段齐全：

```jsonc
{
  "account":      {"id": "...", "label": "hid_xxx"},
  "domainId":     "...",
  "accessKey":    "HSTA...",          // STS 临时 AK（约 2 小时）
  "secretKey":    "...",
  "securitytoken":"...",
  "refresh_token":"eyJ0eXAi...",      // JWT，约 30 天
  "expires_at":   "2026-10-16T09:44:33.108Z",
  "loginContext": {
    "pkcePair":    {"codeVerifier": "...", "codeChallenge": "...", "codeChallengeMethod": "SHA-256"},
    "dpopKeyPair": {"privateKeyJwk": {"kty":"EC","crv":"P-256","x":"...","y":"...","d":"..."},
                    "publicKeyJwk":  {"kty":"EC","crv":"P-256","x":"...","y":"..."}}
  }
}
```

导入到另一台机器 / 容器：

```bash
curl -X POST http://<网关>/admin/api/accounts   -H 'Content-Type: application/json'   -H 'X-Admin-Password: <管理密码>'   -d '{"action":"session-json","name":"portable","session_json":"<粘贴上面整段 JSON>"}'
```

或在控制台「账号 → 从 session JSON 导入」粘贴。

> ⚠️ **这份 JSON 等同于账号登录态。** 含 `refresh_token`（约 30 天）与 DPoP 私钥，
> 拿到它就能以你的身份调用上游。请勿提交到版本库、勿外传、勿贴到聊天里。
> 用完后删掉。
>
> 另外：`codearts2api-data.json` 里本来就缓存着同样的材料（IDE 关闭时续期用），
> 所以导出并不会让暴露面变大 —— 但**别把它复制得到处都是**。

### 3.3 本地浏览器登录（OAuth 回调）

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

#### ⚠️ 远端部署（服务器上的网关 + 本地浏览器）

回调地址在协议里**写死**为 `http://127.0.0.1:<port>/oauth/callback`：

```js
// authorize URL 只传 port，redirect_uri 由门户自己拼
?uri_scheme=codearts-agent&client_id=codearts-agent&port=<port>&code_challenge=...
// 换凭据时也是写死的
redirect_uri: `http://127.0.0.1:${port}/oauth/callback`
```

这是 OAuth 的 **native app / loopback 重定向**（RFC 8252）——
`codearts-agent` 这个 client_id 注册的就是「只允许环回地址」，
所以**你的域名不可能成为回调地址**，CF / Nginx 反代也解决不了。
浏览器永远把 `127.0.0.1` 解析到**它自己那台机器**。

三条路，按推荐顺序：

##### 方案 A：SSH 本地端口转发（推荐）

把回调端口固定下来，配一条常驻隧道：

```bash
# 控制台「设置 → 浏览器登录回调端口」填 64361（0=随机，建议固定）
# 然后在【你本机】执行并保持不断：
ssh -N -L 64361:127.0.0.1:64361 user@your-server
```

之后点「打开授权页」，浏览器访问 `127.0.0.1:64361` 会被隧道送到服务器，
整个流程和本地部署完全一样。控制台会直接把这条命令显示出来，可一键复制。

##### 方案 B：粘贴回调 URL（无需隧道）

1. 正常点「打开授权页」在本地浏览器完成登录
2. 门户会把浏览器跳到 `http://127.0.0.1:<port>/oauth/callback?...`，
   浏览器报「无法访问此网站」—— **但地址栏里的 URL 是完整的**
3. 把那条 URL 整段复制，粘到控制台的「提交回调 URL」

网关会**自己把剩下的重定向链走完**（含门户的两跳
`?secret=&redirect=` → `?code=`）并取出授权码。

##### 方案 C：本地登录 + 导出 session JSON

在任意一台 Windows 机器（或本地跑一个网关实例）完成登录，
按 [3.2](#32-导出-session-json) 导出，再导进服务器。
**不需要任何隧道**，而且导入后仍能自动续期。

> 💡 另外：如果你只是想要一个能用的凭据，方案 C 最省事 ——
> 毕竟会话本来就是可移植的（带 refresh_token + DPoP 私钥）。

### 3.4 续期策略

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

### 3.5 跨平台 / Docker

DPAPI 只有 Windows 有，所以在 Linux / macOS / Docker 里无法自动读取桌面端凭据。
此时有两种方式：

1. **从 session JSON 导入**（推荐）—— 在 Windows 机器上按 [3.2](#32-导出-session-json)
   导出，粘贴过来即可；带着 refresh_token 与 DPoP 私钥，**导入后仍能自动续期**
2. **手工录入** —— 填 STS 临时 AK / SK / Security Token（约 2 小时，之后需要重新填）；
   或在「凭据类型」里选**永久 AK/SK**（IAM 永久凭据，不过期、不续期、不需要 Security Token）

> 容器部署时 `docker-compose.yml` 已带 `-no-import`，不会去扫桌面端目录。

---

## 四、控制台

<http://127.0.0.1:10087/>，三个内嵌文件（HTML + CSS + JS）、无外部依赖、可离线使用。

- **概览**：请求统计、今日领取 n/m、**活动福利列表与邀请链接（可按账号切换查看）**、模型清单（含积分倍率与**逐模型测活**）、一键刷新模型
- **账号**：导入 / **浏览器登录** / 手工录入 / 从 session JSON 导入 / **导出 session JSON** / 探活 / **领取活动福利** / 重读 / 启停 / 删除，AK 与域 ID 脱敏展示，并显示「今日领取」状态
- **密钥**：签发 `sk-ca-...` 调用密钥（未签发时 `/v1/*` **仅允许本机直连**）
- **日志**：只记元信息（模型、耗时、入出长度），不落对话内容
- **设置**：默认 / 强制模型、**允许真实消耗积分开关**、**允许匿名调用开关**、**按密钥限流开关与阈值**、
  **活动福利自动领取开关与时刻**、
  **浏览器登录回调端口**、超时、重试、日志条数、CORS、续期开关、
  上游身份头、模型别名、管理密码（含「清除管理密码」按钮）

鉴权规则：设了管理密码就必须带 `X-Admin-Password`（或 `Authorization: Bearer`）；
没设密码则只允许本机直连访问。所有写操作额外做同源校验。
详见[第八节 安全说明](#八安全说明)。

---

## 五、HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/health` | 探活 + 账号就绪数 |
| GET | `/v1/models` | 模型清单（OpenAI 格式，附带 `codearts` 扩展字段） |
| POST | `/v1/chat/completions` | OpenAI 兼容，支持流式 / 工具调用 / `reasoning_content` |
| POST | `/v1/messages` | Anthropic Messages API 兼容（含 `thinking` / `tool_use` 双向映射） |
| GET | `/metrics` | Prometheus 文本格式运行指标（与管理接口同一套鉴权） |
| GET/POST/DELETE | `/admin/api/*` | 控制台 API（见下表） |

控制台 API：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/admin/api/state` | 首屏状态（账号 / 密钥 / 模型 / 设置 / 统计 / 今日领取） |
| POST | `/admin/api/accounts` | 账号操作：`import` / `manual` / **`export`** / `session-json` / `remove` / `toggle` / `clear-errors` / `reimport` |
| GET/POST/DELETE | `/admin/api/keys` | API 密钥增删改查 |
| GET/DELETE | `/admin/api/logs` `/admin/api/stats` | 请求日志与统计 |
| GET/POST | `/admin/api/settings` | 运行设置 |
| POST | `/admin/api/checkin` | 账号连通性探活（用默认模型发一次 `max_tokens=8`，消耗极少积分） |
| GET | `/admin/api/credits` | **账号积分额度**（套餐总额 / 已用 / 剩余 / 到期）。可选 `?id=<账号>` 只查一个、`?fresh=1` 跳过 60 秒缓存；不消耗积分 |
| POST | `/admin/api/models/probe` | **逐模型测活**：对每个模型发一次 `max_tokens=8` 的真实请求（**真实扣积分**）。可选 `{"models":[...],"id":"<账号>"}`；单次上限 64 个模型，同一时刻只允许一个测活任务（并发时返回 `409`） |
| POST | `/admin/api/claim` | **领取活动福利**：运营活动奖励（全部账号或指定 `id`；免费福利 Token 已随通道下线移除） |
| GET | `/admin/api/welfare` | 运营活动清单与可领取状态（**按账号查询**：`?id=<账号>`；响应带 `accounts` 列表与 `claimable_count`，不带 `id` 时取调度池的下一个账号） |
| POST | `/admin/api/invite` | 生成专属邀请码与邀请链接（奖励仍需好友真实注册） |
| POST | `/admin/api/models/refresh` | 从上游刷新模型清单 |
| POST | `/admin/api/login/start` | **启动浏览器登录**，返回授权 URL + 回调地址 + SSH 隧道命令 |
| POST | `/admin/api/login/callback` | **提交手工粘贴的回调 URL**（远端部署无隧道时用） |
| POST | `/admin/api/login/poll` | 轮询登录状态，成功后自动入库 |
| POST | `/admin/api/login/cancel` | 取消浏览器登录 |

上游返回的 `reasoning_content`（思维链）会原样透传，
OpenAI 侧在 `choices[].message.reasoning_content` / `delta.reasoning_content`，
Anthropic 侧映射为 `thinking` 内容块。

### 错误语义

网关不再把所有失败都报成 `502`，而是尽量保留语义，客户端可以据此决定是否退避重试：

| 情况 | HTTP | OpenAI `error.type` | Anthropic `error.type` |
| --- | --- | --- | --- |
| **网关侧限流**（本机密钥打太快） | `429` | `rate_limit_error` | `rate_limit_error` |
| 上游并发会话打满 / 429 限流 | `429` | `rate_limit_error` | `rate_limit_error` |
| 请求参数或模型名不合法（含上游 `200 + error_code` 的业务错误） | `400` | `invalid_request_error` | `invalid_request_error` |
| 模型不存在 | `404` | `invalid_request_error` | `invalid_request_error` |
| 上游 5xx / 网络错误 | `502` | `upstream_error` | `overloaded_error` |
| **账号登录态失效**（凭据过期且无法续期） | `502` | `credentials_error` | `authentication_error` |
| 账号池为空 | `503` | `server_error` | `overloaded_error` |

> 上游把「并发会话数已达上限」表达为 `HTTP 400 + TM.00001041`，网关会先按
> 临时性故障**退避重试**（不计入账号失败），重试仍失败才回 `429`。
>
> **请求本身不合法时不换号重试、也不计账号故障。** 参数越界（`temperature=99`、
> `max_tokens` 超限、tools 超长）换哪个账号都一样，网关直接把上游的
> `details[].error_msg` 带回来，账号保持 `ok`、`failures` 不变。
> 反过来，`APIG.*` / `STS5.*` 这类凭据错误即使以 `400` 返回，仍会计入账号故障。
>
> **网关侧限流与上游 429 是两件事**：前者是「这个密钥自己打得太快」，响应会带
> `Retry-After`（网关能算准补满令牌的时间）；后者是「账号的并发会话满了」，只能等或换账号。
> 被网关拦下的请求**不会**进入上游，因此不消耗积分。
>
> **账号登录态失效**是唯一需要人工介入的错误：网关会把上游的错误码
> （`STS5.1806` / `invalid_grant` / `APIG.0301` …）翻译成一句人话，
> 并明确提示「请重新登录 CodeArts Agent」。它**不会**报成
> `overloaded_error`，以免误导客户端反复重试。

---

## 六、工程结构

```text
CodeArts2API/
├─ main.go
├─ Makefile              # make check / build / race / docker
├─ .github/workflows/    # CI：gofmt + vet + test + race + 控制台检查
├─ cmd/probe/            # 本机诊断工具
├─ internal/
│  ├─ codearts/          # 上游复刻层
│  │  ├─ session.go            # Session / Account 模型
│  │  ├─ session_windows.go    # DPAPI + AES-GCM 读取与安全回写
│  │  ├─ session_other.go      # 非 Windows 降级
│  │  ├─ sqlite.go             # 极简 SQLite 只读 + 定长原地写
│  │  ├─ signer.go             # 华为云 SDK-HMAC-SHA256
│  │  ├─ client.go             # inferhub 转发 / 模型清单 / 探活
│  │  ├─ refresh.go            # DPoP(ES256 JWS) 续期
│  │  ├─ login.go              # 本地浏览器登录（OAuth PKCE + 回调服务器）
│  │  ├─ welfare.go            # 运营活动（每日积分 / 邀请码）
│  │  ├─ sse.go                # SSE 聚合（流式→非流式兜底）+ 错误分类
│  │  ├─ manager.go            # 凭据解析与续期编排
│  │  ├─ util.go               # 脱敏（Mask）/ 随机数 / 时间等小工具
│  │  └─ debug.go              # state.vscdb 键名转储
│  ├─ registry/          # 模型清单与别名解析
│  ├─ protocol/          # Anthropic ↔ OpenAI 纯数据转换（可脱离网络单测）
│  ├─ ratelimit/         # 按密钥的令牌桶 + 在飞并发闸门（自我保护限流）
│  ├─ apierr/            # 内部错误 → 对外 HTTP 状态码 / OpenAI·Anthropic type
│  ├─ server/            # OpenAI / Anthropic 兼容层 + 控制台
│  │  ├─ chat.go  anthropic.go  models.go  model_resolve.go
│  │  ├─ claim.go              # 每日自动领取活动福利调度
│  │  ├─ login.go              # 浏览器登录 API
│  │  ├─ metrics.go            # /metrics（Prometheus 文本，复用现有统计）
│  │  ├─ admin.go  server.go  web.go
│  │  └─ web/                  # 控制台（三个文件，go:embed 内嵌）
│  │     ├─ index.html         # 结构 + 语义（无内联 style / script）
│  │     ├─ console.css        # 样式与工具类
│  │     └─ console.js         # 交互逻辑
│  └─ store/             # APIKey / 日志 / 设置 / 账号池（含 schema 版本与迁移）
├─ tools/                # 辅助脚本（Python）
│  ├─ decrypt_session.py  # 只读解密 state.vscdb，验证凭据能否读到
│  ├─ probe_api.py        # 直接打上游的签名 PoC
│  └─ check_html_js.py    # 校验控制台 JS（跨行字符串 / node --check）与 CSS（括号 / 外部资源）
└─ docs/
   ├─ index.md            # 文档导航
   ├─ UPSTREAM.md         # 当前上游协议事实（活文档）
   ├─ TROUBLESHOOTING.md  # 故障排查（症状 → 原因 → 处置）
   └─ RECON.md            # 逆向勘察报告（历史快照）
```

---

## 七、开发与自检

```bash
# 一条命令跑完提交前检查（vet + 跨平台 vet + test + 控制台 JS/CSS）
make check

# 或逐项跑
go build ./... && go vet ./... && go test ./...

# 校验控制台 JS（跨行字符串 / node --check）与 CSS（括号配对 / 无外部资源）
python tools/check_html_js.py

# 本机诊断：凭据 / 模型清单 / 活动福利（加 -model 才做对话探活，会消耗少量积分）
go run ./cmd/probe

# 并发竞态检查（需要 cgo；Windows 下注意下面的说明）
go test -race ./...
```

> **在 Windows 上跑 `-race`**：`-race` 需要 cgo，而 Go 会调用 `gcc` 来链接，
> `gcc` 内部又去找 `ld`。scoop 装的 mingw 只提供了 `ld.bfd.exe`，没有 `ld.exe`，
> 于是报 `collect2.exe: fatal error: cannot find 'ld'`。做一个同名副本即可：
>
> ```bash
> mkdir -p /tmp/ldshim
> cp "$HOME/scoop/apps/mingw/current/bin/ld.bfd.exe" /tmp/ldshim/ld.exe
> PATH="/tmp/ldshim:$PATH" go test -race ./...
> ```
>
> `internal/store` 的 `TestConcurrentAccountAccessIsSafe` 就是为 `-race` 写的：
> 它并发跑「控制台读账号快照 + 请求路径写账号字段」，能直接抓到
> 「对外暴露内部 `*Account` 指针」这类数据竞争。
>
> 控制台只有一个脚本文件（`web/console.js`），**任何一处语法错误都会让整段脚本不执行** ——
> 按钮、事件绑定、初始加载全部失效，表现是「能输密码但点什么都没反应」。
> 最容易踩的两个坑：把真实换行打进字符串字面量（JS 的 `"..."` 不能跨行），
> 以及把 `on("#x", function(){...});` 写成 `on("#x", function(){...};`。
> `go test ./internal/server/` 里有对应守卫：`TestConsoleScriptHasNoMultilineString`
> （跨行字符串）、`TestConsoleScriptBalanced`（括号配对）、
> `TestConsoleHTMLHasNoInlineCode`（无内联 style/script）与
> `TestConsoleDOMContract`（脚本引用的元素 id 必须存在于 HTML）。

---

## 八、安全说明

### 访问控制模型

网关把「本机直连」作为两条兜底鉴权的共同前提：

```text
本机直连 = RemoteAddr 是环回地址  且  请求未携带任何代理转发头
           (X-Forwarded-For / X-Real-Ip / Forwarded / X-Forwarded-Proto …)
```

| 接口 | 未配置凭据时的行为 |
| --- | --- |
| `/admin/api/*` | 未设管理密码 → **仅本机直连**；设了密码 → 任何来源都必须携带密码 |
| `/v1/*` | 未签发密钥 → **仅本机直连**（可在设置里打开「允许匿名调用」放开）；签发了密钥 → 必须携带有效密钥 |
| `/health` `/ping` `/` | 不需要鉴权（只返回计数与静态页面） |

> **为什么要看代理头**：同机反向代理（`proxy_pass 127.0.0.1:10087`）会把
> **所有**公网请求的 `RemoteAddr` 变成 `127.0.0.1`。只看 `RemoteAddr`
> 等于把管理接口（含导出完整登录态）暴露给公网，所以带代理头的请求
> 一律按「非本机」处理。
>
> **容器场景**：容器内的网关看到的来源是 docker 网桥地址（不是环回），
> 因此通过端口映射使用控制台时**必须设置管理密码**；
> 需要让宿主机客户端免密钥调用 `/v1/*` 时再打开「允许匿名调用」。

### 其它

- 状态文件（`codearts2api-data.json`）里含 STS 凭据与 `refresh_token`，
  以 `0600` 创建、原子替换（唯一临时文件 + 串行落盘）；
  请勿提交到版本库（见 `.gitignore`）。注意 Windows 上 `0600` 不映射到 ACL。
- 控制台对 AK / 域 ID / APIKey 一律脱敏展示，`Authorization` 与 `X-Security-token`
  从不写进日志。控制台页面全程用 `textContent` 渲染。
- 控制台**不含任何内联脚本 / 内联样式**，因此 CSP 可以收紧到
  `script-src 'self'; style-src 'self'`（无 `'unsafe-inline'`），
  配合 `default-src 'none'`、`connect-src 'self'`、`frame-ancestors 'none'`：
  即使将来引入注入点，脚本执行不了、数据也运不出去。
- 管理密码不再写入 `localStorage`（明文落盘等价于「导出完整登录态」的权限），
  只在内存与当前标签页的 `sessionStorage` 中保存；历史版本残留的值会在启动时清除。
- **CORS 默认只放行同源**（控制台自己的页面），不再默认放行任意 localhost 端口。
- **按密钥限流默认开启**（QPS 5 / 突发 10 / 并发 8）：只做**自我保护**，
  防止失控客户端（循环重试、Agent 死循环）烧光账号积分。被拦下的请求在调用上游前
  就返回 `429`，**不消耗额度**；不承诺密钥之间的公平配额（见 `goal-and-requirements.md` 的非目标）。
  需要别的来源时在设置里显式列出，且不接受通配符。
- 上游身份头里的 `x-ot-session-id` 按凭据推导并**复用**（不再每请求随机），
  否则会持续撞上游「每账号 3 个并发会话」的上限。
- 网关默认只监听 `127.0.0.1`。要对外提供服务请显式设置管理密码与
  `CORS 允许来源`，并在反向代理层再做一层鉴权。
- 完整的威胁模型、残余风险与部署前加固清单见 [`SECURITY.md`](SECURITY.md)。
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
