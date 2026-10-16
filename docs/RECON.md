# CodeArts2API 逆向勘察报告

> 目标：把本机 **CodeArts Agent**（华为云码道 / CodeArts Doer for Coding）桌面端的
> 上游模型服务，复刻成一个 OpenAI 兼容网关。
>
> 结论：**可行，且比参考项目 Doubao2API 简单** —— 上游本身就是 OpenAI 兼容协议，
> 不需要像豆包那样把私有补丁流还原成文本。

> **本报告已被实现。** 对应代码见仓库根目录的 `internal/codearts/`（凭据 / 签名 / 转发 / 续期）
> 与 `internal/server/`（OpenAI + Anthropic 兼容层与控制台）；使用方式见 `README.md`。

---

## 一、结论速览

| 环节 | 逆向结果 | 难度 |
| --- | --- | --- |
| 应用形态 | **VS Code 1.109.5 fork**（Electron），`resources/app/out/main.js` | 低 |
| AI 能力载体 | 内置扩展 `huaweicloud.vscode-codebot`（package name `codearts-snap`，v26.9.902） | 低 |
| Agent 内核 | `agentkernelServer-win32-x64.exe`（173 MB，**Bun 单文件 exe**，基于 opencode 二次开发） | — |
| 推理入口 | `POST https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions` | — |
| 协议 | **OpenAI 兼容**（`chat.completion` / `chat.completion.chunk` SSE），额外带 `reasoning_content` | **极低** |
| 鉴权 | 华为云 **SDK-HMAC-SHA256** 签名 + `X-Security-token`（STS 临时凭证） | 中 |
| 凭据落盘 | `%APPDATA%\codearts-agent\User\globalStorage\state.vscdb`（SQLite） | 低 |
| 凭据加密 | **两层**：DPAPI 保护的主密钥（`Local State`）+ AES-256-GCM 逐条加密（`v10` 前缀） | 低（与 Doubao2API 同源） |
| 模型列表 | `GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config` | 低 |
| 额度查询 | `GET https://opengw.developer.huaweicloud.com/api/v1/user/tokens/balance` | 低 |
| 续期 | `POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens`（DPoP JWS + refresh_token） | 中 |

**实测已跑通**：模型列表 / 额度 / 非流式对话 / 流式 SSE / Function Calling，见 `tools/`。

---

## 二、链路

```text
┌──────────────┐  /v1/chat/completions  ┌──────────────┐  SDK-HMAC-SHA256 签名  ┌──────────────────────────────────────┐
│ OpenAI SDK   │  /v1/models            │ CodeArts2API │  + X-Security-token    │ snap-access.cn-north-4.myhuaweicloud │
│ NewAPI / pi  │ ─────────────────────▶ │   (本网关)   │ ─────────────────────▶ │ .com/api/v2/chat/completions         │
└──────────────┘  SSE 流式 / 非流式      └──────┬───────┘      OpenAI SSE        └──────────────────────────────────────┘
                                               │ 只读解密
                                               ▼
                    %APPDATA%\codearts-agent\Local State                 ← DPAPI 主密钥
                    %APPDATA%\codearts-agent\User\globalStorage\state.vscdb
                       secret://{"extensionId":"huaweicloud.authentication",
                                 "key":"HuaweiCloudSession"}               ← AK/SK/STS/refresh_token
```

和 Doubao2API 最大的差别：**上游协议不用转换**。豆包那边要把 `content_block` 补丁流
还原成文本 / 思维链；CodeArts 这边上游直接吐标准 `chat.completion.chunk`。

---

## 三、关键事实与证据位置

### 3.1 应用形态

`F:\IDE\CodeArts Agent\resources\app\product.json`

```
nameShort            codearts-agent          vscodeVersion  1.109.5
dataFolderName       .codearts-doer-for-coding
snapEngineUrl        https://snap-access.cn-north-4.myhuaweicloud.com
apiHost              https://ide.cn-north-4.myhuaweicloud.com
urlProtocol          codearts-agent          ← OAuth client_id
```

### 3.2 端点定义

`resources/app/extensions/vscode-codebot/product.json` → `commercialVersionDomain.newFramework.productDomain`

```
snapApigDomain    https://snap-access.cn-north-4.myhuaweicloud.com   (备用 .cn)
snapApigBackupDomain  https://snap-access.cn-north-4.myhuaweicloud.cn
codeGenDomain / chatDomain / policyCenterDomain / agentModeDomain / memoryServiceDomain
                 均 = snapApigDomain
iamStsOpenDomain https://sts.cn-north-4.myhuaweicloud.com
OTDomain         https://collector-agents-access.cn-north-4.myhuaweicloud.com/collector
```

`out/extension.js` 里拼出的 URL：

```js
this.chatUrl          = `${chatDomain}/v1/chat/chat`          // 旧框架，不用
this.codeGenUrl       = `${codeGenDomain}/v1/code/complete`   // 旧框架，不用
// 新框架（Agent Kernel）
inferhubBaseUrl       = `${agentChatDomain}/api/v2/`          → /api/v2/chat/completions
```

### 3.3 Agent 内核（opencode 二次开发）

`agentkernelServer-win32-x64.exe` 内的关键片段：

```js
function CA(n){let t=n.replace(/\/$/,"");
  if(t.endsWith("/chat/completions"))return t;
  return `${t}/chat/completions`}                      // → /api/v2/chat/completions

// 凭据优先级：global config userToken > env W3_TOKEN
function FA(n){let t=n?.userToken?.trim(); if(t)return{token:t,source:"global_userToken"};
  let e=process.env.W3_TOKEN?.trim(); if(e)return{token:e,source:"env_W3_TOKEN"};
  return{source:"none"}}

// 真正发请求时走的是 HC 签名分支（PLUGIN_ENV=hc）
//   resolveAkSk(): GlobalConfig.userInfo.{accessKeyId,secretAccessKey,accessToken}
//   Th.sign(req, new Uh(ak, sk))   ← 华为云 SDK-HMAC-SHA256
```

`out/extension.js` 把配置推给内核：`POST/PATCH /v1/config`、`/cag/global/config`，
body 字段含 `userName / userToken / inferhubBaseUrl / policyCenterDomain / ...`。

### 3.4 真实抓包（内核日志）

`~/.codeartsdoer/codearts-data/log/kernel-codeartsdoer-incognito-*.log`

```
service=inferhub-provider "[header-debug] full headers: {
  "x-ot-trace-id": "...", "x-ot-span-id": "...", "x-snap-traceid": "...",
  "x-ot-session-id": "ses_...", "user-session-id": "ses_...",
  "x-ot-function": "agent-general", "maas_type": "benefit", "X-Language": "zh-cn",
  "user-msg-id": "msg_...", "created-time": "2026-10-16T01:37:44Z",
  "model-id": "glm-5.3-flash", "model-name": "glm-5.3-flash",
  "x-ot-client-type": "IDE", "x-ot-client-version": "26.9.902",
  "plugin-name": "codemate_vscode", "plugin-version": "26.9.902",
  "ide-name": "VSCode", "ide-version": "26.9.902",
  "client-ip": "198.18.0.1", "X-Sdk-Date": "20261016T013744Z",
  "host": "snap-access.cn-north-4.myhuaweicloud.com" }"
   ← 日志里被过滤掉的两个头就是 X-Security-token 和 Authorization
```

### 3.5 签名算法（必须逐字对齐）

`out/extension.js` 中打包的 `huaweicloud-sdk-core` signer：

```js
function d(O,N){ ... return O.method+`\n`+p(O)+`\n`+g(O)+`\n`+m(O,N)+`\n`+N.join(";")+`\n`+M }

// ★ CanonicalURI：每段单独 urlencode，且强制补尾斜杠
function p(O){ let N=O.uri.split("/"), M=[]; for(let Q of N) M.push(c(Q));
               let k=M.join("/"); return k[k.length-1]!=="/" && (k=k+"/"), k }

// ★ CanonicalHeaders：每个 k:v.trim() 用 \n 连接后自带尾 \n，
//   外层 d() 又补了一个 \n —— 于是 signed headers 前有一个空行
function m(O,N){ ... k.push(Q+":"+U.trim()); return k.join(`\n`)+`\n` }

// 所有请求头都参与签名（含 x-security-token）
function y(O){ ... N.push(M.toLowerCase()); return N.sort(), N }
```

**两个坑**（PoC 里已复现验证）：

1. CanonicalURI 末尾补 `/`：`/api/v2/chat/completions` → `/api/v2/chat/completions/`
2. CanonicalHeaders 与 SignedHeaders 之间是**两个** `\n`（一个空行）

### 3.6 凭据存储

```
%APPDATA%\codearts-agent\Local State
  os_crypt.encrypted_key = base64("DPAPI" + CryptProtectData(AES-256 key))

%APPDATA%\codearts-agent\User\globalStorage\state.vscdb  (SQLite, ItemTable)
  key   secret://{"extensionId":"huaweicloud.authentication","key":"HuaweiCloudSession"}
  value {"type":"Buffer","data":[118,49,48,...]}   # "v10" + nonce(12) + AES-256-GCM(ct||tag)
```

解密后的 session 结构：

```json
{
  "account": {"id": "...", "label": "hid_..."},
  "accessKey": "HSTA...",            // STS 临时 AK
  "secretKey": "...",                // STS 临时 SK
  "securitytoken": "...",            // X-Security-token
  "accessToken": "",                 // 商业版为空（IAM token 走签名，不用它）
  "expires_at": "2026-10-16T03:32:19.788Z",   // 约 2 小时
  "domainId": "...",
  "refresh_token": "eyJ0eXAi...",    // JWT，约 30 天
  "loginContext": {
    "pkcePair": {"codeVerifier": "...", "codeChallenge": "..."},
    "dpopKeyPair": {"privateKeyJwk": {"kty":"EC","crv":"P-256","x":"...","y":"...","d":"..."},
                    "publicKeyJwk": {...}}
  },
  "id": "huawei-cloud-authentication"
}
```

> 提供者 ID 是 `huawei-cloud-authentication`（`state.vscdb` 里有
> `huawei-cloud-authentication-hid_xxx-usages`），但 secret key 挂在
> `huaweicloud.authentication` 扩展下。

### 3.7 模型与额度

```
GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config
→ {"base_url":"https://opengw.developer.huaweicloud.com/v2",
   "models":[{"model_id":"deepseek-v4-flash-0731","context_window":1048576,"max_tokens":393216,"sort":10},
             {"model_id":"glm-5.3-flash","context_window":1048576,"max_tokens":131072,"sort":20},
             {"model_id":"deepseek-v4-pro-0813","context_window":1000000,"max_tokens":393216,"sort":30},
             {"model_id":"deepseek-v4.1-flash","context_window":1000000,"max_tokens":384000,"sort":40}]}

GET https://opengw.developer.huaweicloud.com/api/v1/user/tokens/balance
→ {"channel":"codearts","daily_token_limit":10000000,"daily_tokens_used":1234567,
   "total_balance":8765433,"total_quota":10000000}
```

另有一条 **builtin** 列表（企业/订阅模型，走 `Agent-Type: PromptCenter`）：

```
GET https://snap-access.cn-north-4.myhuaweicloud.com/v1/model/builtin
→ GLM-5.2 / Qwen3-VL-235B / kimi-k2.6-vl / openpangu-2.0-flash /
  openpangu-2.0-pro / glm-5.3 / glm-5.2-sft-harmony
```

> 注意：`/api/v2/chat/completions` 只认 **benefit 通道**的模型名（`glm-5.3-flash`），
> 传 `glm-5.3` 会返回 `{"error_code":"4005","error_msg":"unsupported model"}`（HTTP 200）。
> 不带 `maas_type: benefit` 则 404 `Route missed for model`。

### 3.8 续期流程（代码已确认，未实跑）

`extensions/huaweicloud.authentication/dist/plugin.js`

```js
CLIENT_ID   = env.uriScheme                       // "codearts-agent"
IAM_TOKEN_API = "/v1/oauth2/tokens"
iamStsHost  = "https://sts.cn-north-4.myhuaweicloud.com"

refreshToken(e,t){ requestToken(e, {
    client_id:     CLIENT_ID,
    code_verifier: e.pkcePair.codeVerifier,
    grant_type:    "refresh_token",
    refresh_token: t }) }

generateDpopPayload(htm,htu) => { htm, htu, iat, jti }
generateDpopJWE(pair,payload) =>
    new SignJWT(payload).setProtectedHeader({alg:"ES256", typ:"dpop+jwt", jwk: pair.publicKeyJwk})
                        .sign(importJWK(pair.privateKeyJwk,"ES256"))
// POST，headers: { DPoP: <jws>, Content-Type: application/x-www-form-urlencoded }
// 响应: { credentials: {access_key_id, secret_access_key, security_token, expiration},
//         refresh_token }
```

`applyRenewedToken` 里读的正是 `e.credentials.access_key_id / secret_access_key /
security_token / expiration` + `e.refresh_token`。

---

## 四、实测结果（PoC）

`tools/decrypt_session.py` + `tools/probe_api.py` 均已跑通：

```
$ python tools/probe_api.py --tools
账号: hid_xxxxxxxxxxxx | 过期: 2026-01-01T03:32:19.788Z
== 可用模型 == 200   (4 个 benefit 模型)
== 额度 == 200       daily_tokens_used=1234567 / 10000000
== 对话 == 200
{"id":"as-vgdmew7p4h","object":"chat.completion","model":"glm-5.3-flash",
 "choices":[{"index":0,"message":{"role":"assistant","content":"你好",
   "reasoning_content":"The user asked me to reply with only two characters..."},
   "finish_reason":"stop"}],
 "usage":{"prompt_tokens":156,"completion_tokens":40,"total_tokens":196,
          "completion_tokens_details":{"reasoning_tokens":37}}}
```

Function Calling 也 OK：

```json
{"choices":[{"index":0,"message":{"role":"assistant","content":"",
  "tool_calls":[{"index":0,"id":"chatcmpl-tool-...","type":"function",
    "function":{"name":"get_weather","arguments":"{\"city\": \"北京\"}"}}],
  "reasoning_content":"..."},"finish_reason":"tool_calls"}]}
```

流式 SSE 也是标准 `chat.completion.chunk`：

```
data:{"id":"as-pn2czaefth","object":"chat.completion.chunk",...,"delta":{"role":"assistant","content":"","reasoning_content":"The"}}
```

---

## 五、与 Doubao2API 的对照

| 维度 | Doubao2API | CodeArts2API（本方案） |
| --- | --- | --- |
| 应用形态 | Chromium 原生（Lark 内核，`biz.pak` 224 MB） | **Electron（VS Code fork，`app.asar` 未打包，源码基本裸露）** |
| 凭据落盘 | SQLite Cookies + DPAPI + AES-GCM + host 前缀 | **SQLite secret + DPAPI + AES-GCM（同源，无 host 前缀）** |
| 凭据解析 | 手写 SQLite b-tree 解析器（零依赖） | **同样可手写；或直接读 `ItemTable` 单行** |
| 上游协议 | 私有 SSE（`content_block` 补丁流），需还原 | **OpenAI 原生，零转换** |
| 上游鉴权 | Cookie + `x-tt-passport-csrf-token` | **SDK-HMAC-SHA256 签名 + `X-Security-token`** |
| 凭据有效期 | 长（Cookie） | **2 小时，需要续期** |
| 逆向难度 | 高（要拆 pak + source map） | **低（有 source map / 有完整日志 / 有明文 JS）** |

**结论：核心工作量比 Doubao2API 小，唯一的额外件是「华为云签名器 + 凭证续期」。**

---

## 六、落地实现

本报告的结论已全部实现，代码结构如下（每一项都标注了对应的逆向依据）：

```text
CodeArts2API/
├─ main.go                     # 启动 / 参数 / 自动导入
├─ cmd/probe/                  # 本机诊断：凭据 / 模型 / 额度 / 对话探活
├─ internal/
│  ├─ codearts/                # 上游复刻层
│  │  ├─ session.go            #   Session / Account 模型（§3.6 的字段）
│  │  ├─ session_windows.go    #   DPAPI + AES-GCM 读取与安全回写（§3.6）
│  │  ├─ session_other.go      #   非 Windows 降级（手工录入）
│  │  ├─ sqlite.go             #   极简 SQLite 只读 + 定长原地写
│  │  ├─ signer.go             #   SDK-HMAC-SHA256（§3.5 的两个坑）
│  │  ├─ client.go             #   inferhub 转发 / 模型 / 额度（§3.2 §3.7）
│  │  ├─ refresh.go            #   DPoP(ES256 JWS) 续期（§3.8）
│  │  ├─ manager.go            #   凭据三级解析与续期编排
│  │  └─ debug.go              #   state.vscdb 键名转储
│  ├─ registry/                # 模型清单与别名解析（§3.7）
│  ├─ server/                  # OpenAI / Anthropic 兼容层 + 控制台
│  └─ store/                   # APIKey / 日志 / 设置 / 账号池
├─ tools/                      # 逆向 PoC 脚本（Python，与实现相互印证）
└─ docs/RECON.md               # 本报告
```

| 逆向结论 | 实现位置 | 验证方式 |
| --- | --- | --- |
| §3.5 签名两个坑 | `signer.go` | `TestCanonicalURIAddsTrailingSlash`、`TestSignRequestStableVector` |
| §3.6 凭据加密 | `session_windows.go` | `TestReadItemValueFindsSecret` |
| §3.8 DPoP 密钥绑定 | `refresh.go` | `TestJWKThumbprintMatchesRefreshTokenBinding`（比对 `cnf.jkt`） |
| 回写安全性 | `sqlite.go` + `session_windows.go` | `TestPatchItemValueRoundTrip`、`TestWriteBackSessionRoundTrip` |

---

## 七、风险与注意点

1. **凭证续期是唯一的硬骨头**。`refresh_token` 可能是**单次使用 + 轮转**的。
   如果网关自己刷新，IDE 里存的旧 refresh_token 可能失效，导致 IDE 掉登录。
   → 已落地策略：**优先只读 `state.vscdb`**（IDE 活着时它自己会续期，网关按
   10 秒 TTL 跟随），只有在「IDE 未运行且凭证已过期」时才自己走 DPoP 续期，
   并把新 session **安全回写 `state.vscdb`**。
2. **回写必须是无结构风险的**。state.vscdb 是 SQLite，随便写会改坏页结构。
   → 已落地策略：只做**同长度字节覆写**（不足处用 JSON 尾随空格补齐），
   不增删 cell、不动 freelist，写前自动备份 `.codearts2api.bak`；
   长度超出时按设计跳过。
3. **SQLite 溢出页本地长度公式**容易写错：取模基数是
   `M = ((U-12)*32/255)-23` 而不是 `X = U-35`。用错会让带溢出页的记录
   整行读不出来（`state.vscdb` 里那条 secret 就是 10 KB 的溢出记录）。
4. **配额是共享的**：`daily_token_limit = 10,000,000`，IDE 和网关共用同一份额度，
   网关跑太多会把 IDE 当天的额度吃光。
5. **`maas_type: benefit` 是免费权益通道**，模型集合由 `gateway/config` 决定；
   换账号 / 换套餐会变。不带该头会 404 `Route missed for model`。
6. **上游错误可能是 HTTP 200**：未知模型返回
   `{"error_code":"4005","error_msg":"unsupported model"}`，必须识别，
   否则会被当成正常回复透传给客户端。
7. **`snap-access.cn-north-4.myhuaweicloud.com` 有备用域名 `.cn`**，已做故障转移。
8. **日志脱敏**：内核日志会把 `x-auth-token` 截断、`Authorization` 打码，说明官方
   自己也认为这些是敏感信息；网关的控制台与日志同样做了脱敏。
9. **客户端断开不等于账号故障**：流式请求被客户端提前掐断时不应计入账号失败，
   否则账号会被误判为不可用。
