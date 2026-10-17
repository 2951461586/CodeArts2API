# 上游协议参考（活文档）

> 本文件是**当前**上游行为的参考，随代码一起维护。
> 时间点快照、逆向过程与证据位置见 [`RECON.md`](RECON.md) —— 那份是历史记录，
> 不应被当作「现在仍然如此」的依据。两者冲突时以本文件与代码为准，并回头修本文件。
>
> 对齐版本：CodeArts Agent **26.9.902**（`plugin-version` / `ide-version`），
> 观测时间 2026-10。上游随时可能变更。

置信度标记：✅ 实测跑通　🔶 代码确认、未实跑　⚠️ 易变、可能随时失效

---

## 1. 端点

| 用途 | 方法 + 路径 | 主机 | 置信度 |
| --- | --- | --- | --- |
| 推理（OpenAI 兼容） | `POST /api/v2/chat/completions` | `https://snap-access.cn-north-4.myhuaweicloud.com` | ✅ |
| 备用域名 | 同上 | `https://snap-access.cn-north-4.myhuaweicloud.cn` | 🔶 |
| benefit 模型清单 | `GET /api/v1/gateway/config` | `https://opengw.developer.huaweicloud.com` | ✅ |
| 每日额度 | `GET /api/v1/user/tokens/balance` | 同上 | ✅ |
| benefit 通道开关 | `GET /v1/benefit-gateway-config` | 同上 | ✅ |
| 每日免费 Token 领取 | `GET /api/v1/benefit/claim`（幂等） | 同上 | ✅ |
| 订阅/企业内置模型 | `GET /v1/model/builtin`（需 `Agent-Type: PromptCenter`） | `https://snap-access.cn-north-4.myhuaweicloud.com` | ✅ |
| 运营活动清单 | `GET /v1/ops/delivery`（需 `Agent-Type: PromptCenter`） | 同上 | ✅ |
| 运营活动领取 | `POST /v1/ops/claim`（幂等，带 `idempotentKey`） | 同上 | ✅ |
| 邀请码生成 | `POST /v1/ops/invite/generate` | 同上 | ✅ |
| 令牌刷新（OAuth2） | `POST /v1/oauth2/tokens` | `https://sts.cn-north-4.myhuaweicloud.com` | 🔶 |

OAuth 客户端标识：`client_id = codearts-agent`（= 客户端 `product.json` 的 `urlProtocol`）。

对应代码常量集中在 `internal/codearts/client.go` 顶部。**改端点只改那里。**

---

## 2. 身份头

上游会据此做路由、埋点与限流，缺省值与 CodeArts Agent 26.9.902 对齐。

| 头 | 缺省值 | 说明 |
| --- | --- | --- |
| `plugin-name` | `codemate_vscode` | 插件标识 |
| `plugin-version` | `26.9.902` | 插件版本 |
| `ide-name` | `VSCode` | 宿主 IDE |
| `ide-version` | `26.9.902` | 宿主版本 |
| `app-id` | `CodeAgent3.0` | 应用标识 |
| `X-Language` | `zh-cn` | 语言 |
| `x-ot-client-type` | `IDE` | 客户端类型 |
| `x-ot-function` | `agent-general` | 功能路由 |
| `x-ot-session-id` / `user-session-id` | `ses_<hash>` | **必须按凭据复用**，见 §6 |
| `x-ot-trace-id` / `x-ot-span-id` / `x-snap-traceid` | 随机 | 链路追踪 |
| `user-msg-id` | `msg_<random>` | 消息标识 |
| `created-time` | UTC ISO8601 | 请求时间 |
| `model-id` / `model-name` | 模型名 | **必须与请求体 `model` 完全一致**，见 §7 |
| `maas_type` | `benefit` 或省略 | 通道开关，见 §3 |
| `X-Security-token` | STS token | 仅临时凭据时携带 |
| `Authorization` | `SDK-HMAC-SHA256 ...` | 见 §4 |

可在控制台「设置 → 上游身份头」覆盖前四项（`plugin-name` / `plugin-version` /
`ide-name` / `ide-version`），便于上游版本变更后无需改代码即可跟随。

---

## 3. 两条通道：`maas_type` 决定路由与计费

| 通道 | 请求头 | 计费 | 模型清单来源 | 典型模型 |
| --- | --- | --- | --- | --- |
| **benefit** | `maas_type: benefit` | 消耗每日福利 Token（1000 万/日，24:00 清零） | `/api/v1/gateway/config` | `glm-5.3-flash`、`deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`deepseek-v4.1-flash` |
| **credit** | **不带** `maas_type` | 按积分计费（真实消耗） | `/v1/model/builtin` | `glm-5.3`、`GLM-5.2`、`openpangu-2.0-pro` … |
| education / examination | `maas_type: education` 等 | — | — | 需学生认证 / 考试资格，普通账号 403 |

**关键约束：两条通道互不相通。** 福利 Token 用尽后 benefit 模型不可再用，
且**无法识别的模型名必须默认落到 benefit 通道** —— 宁可让上游报 `4005`，
也不能静默切到积分通道扣费。网关的 `registry` 与 `settings.allow_credit_models`
就是为这条约束服务的。

---

## 4. 签名：SDK-HMAC-SHA256

逐字对齐 `@huaweicloud/huaweicloud-sdk-core` 的 JS 实现。**有两处与官方文档不同**，
必须一起复刻，否则上游必然回 `APIG.0301`：

1. **CanonicalURI 每段单独编码后强制补尾斜杠**
   `/api/v2/chat/completions` → `/api/v2/chat/completions/`
2. **CanonicalHeaders 每行自带尾 `\n`，拼装时又补一个 `\n`**
   → `SignedHeaders` 之前多一个空行

拼接顺序：

```text
CanonicalRequest =
  METHOD \n
  CanonicalURI \n
  CanonicalQuery \n
  CanonicalHeaders（每行 "k:v\n"，末尾已含 \n） \n
  SignedHeaders（按字典序，';' 连接） \n
  sha256Hex(payload)

StringToSign =
  "SDK-HMAC-SHA256" \n
  X-Sdk-Date \n
  sha256Hex(CanonicalRequest)

Signature = hex(HMAC-SHA256(SecretKey, StringToSign))
Authorization = SDK-HMAC-SHA256 Access=<AK>, SignedHeaders=<...>, Signature=<...>
```

其他细节：

- URL 编码只保留 `A-Za-z0-9-._~`，其余一律 `%XX` **大写**。
  **不能用 Go 的 `url.PathEscape`** —— 它会放过 `$&+,:;=@`，导致签名不一致。
- 查询串：键、值各自排序后再编码。
- 只有 `PUT` / `PATCH` / `POST` 的 body 参与签名；其余方法按空 body。
- header 名小写化后参与签名；**只对值做 `trim`，不对名字做 `trim`**。

实现：`internal/codearts/signer.go`。
金标准测试：`internal/codearts/signer_test.go` —— 期望值由独立的 Python 复刻算出，
**任何一处 canonical 拼装改动都会让测试失败**。改签名逻辑前先读这个测试。

---

## 5. 凭据存储与解密

```text
%APPDATA%\codearts-agent\Local State
  └─ os_crypt.encrypted_key  → DPAPI 保护的主密钥（Windows）

%APPDATA%\codearts-agent\User\globalStorage\state.vscdb   (SQLite)
  └─ ItemTable['secret://{"extensionId":"huaweicloud.authentication",
                          "key":"HuaweiCloudSession"}']
     └─ 值形如 v10:<12B nonce><ciphertext>  → AES-256-GCM
```

解出来的 JSON 含 `accessKey` / `secretKey` / `securitytoken` / `accessToken` /
`refreshToken` / `loginContext`（DPoP 私钥 + PKCE verifier）/ 过期时间。

**平台限制**：DPAPI 只有 Windows 有。非 Windows 平台无法自动读取，
必须通过控制台「手工录入」或「从 session JSON 导入」（后者带 refresh_token
与 DPoP 私钥，导入后仍可自动续期）。

实现：`internal/codearts/session_windows.go`（Windows）、`session_other.go`（降级）、
`sqlite.go`（极简只读 + 定长原地写回）。

---

## 6. 并发会话上限与 `x-ot-session-id`

上游按 `x-ot-session-id` 统计并发会话，**每账号上限 3 个**。超出时返回
`HTTP 400 + error_code TM.00001041`。

因此网关必须按凭据**推导并复用**同一个 session id（`ses_<hash(domainID|AK)>`），
而不是每请求随机。凭据轮换（AK 变化）后自然换新。
见 `Session.UpstreamSessionID()`。

---

## 7. 上游怪癖

### 7.1 `glm-5.3` 只支持流式

```text
glm-5.3  stream=false  → HTTP 400，body 长度 0
glm-5.3  stream=true   → 200，正常 SSE（含工具调用）
```

网关对策：检测到「4xx + 空 body」自动改用流式，本地聚合后以非流式返回；
并把该模型标记为 `stream_only`，后续直接走流式。见 `registry.Model.StreamOnly`。

### 7.2 模型名必须与请求头完全一致

```text
body.model = "glm-5.3 "（尾随空格）
→ 406 {"error_code":"InferHub.001001011.406","error_msg":"The model not equals requestHeader's Model-Id"}
```

改写模型名时（别名 / 强制模型）必须同时改写 `model-id` 与 `model-name` 两个头。
见 `rewriteModel()` 与 `Client.baseHeaders()`。

### 7.3 业务错误用 HTTP 200 表达

上游可能返回 `200 + {"error_code":"4005","error_msg":"unsupported model"}`。
这属于客户端错误，网关会转成 `400`，而不是当成成功响应。

### 7.4 领取类接口是幂等的

`/api/v1/benefit/claim` 当天重复调用仍返回 `0000 success`，额度不变。
因此「每日自动签到」可以放心按本地日期轮询，重复执行无副作用。

---

## 8. 错误码对照

| 上游错误码 | 含义 | 网关处理 |
| --- | --- | --- |
| `TM.00001041` | 并发会话数已达上限（HTTP 400） | 按临时故障退避重试（**不计入账号失败**），仍失败则回 `429` |
| `APIG.0301` / `APIG.0303` | 签名校验失败 / 凭据失效 | 翻译成「登录态失效，请重新登录」→ `502 credentials_error` |
| `STS5.1804` | refresh_token 已过期 | 同上 |
| `STS5.1806` | refresh_token 已被使用（单次 + 轮转） | 同上；提示「IDE 与网关同时续期会互相作废」 |
| `invalid_grant` | refresh_token 失效 | 同上 |
| `4005` | 该通道不支持此模型 | `400 invalid_request_error` |
| `InferHub.001001011.406` | 模型名与 `model-id` 不一致 | `400 invalid_request_error` |
| `TM.00001027` | 未通过学生认证（education 通道） | `400`（普通账号不应命中） |

**凭据失效是唯一需要人工介入的错误**，网关会明确提示「请重新登录 CodeArts Agent」，
并刻意**不**报成 `overloaded_error`，避免客户端无限重试。

---

## 9. 如何复核（上游变更后）

```bash
# 1) 直连上游的签名 PoC（不经过网关）
python tools/decrypt_session.py          # 先确认凭据能读出来
python tools/probe_api.py                # 签名直打上游：模型 / 额度 / 对话

# 2) 经过网关的端到端
go run ./cmd/probe

# 3) 本机 session → 2API 全能力（非流式/流式/思维链/工具调用/Anthropic）
python tools/local_session_e2e.py
```

## 10. 变更流程

上游行为变化时：

1. 先改本文件（记录新事实与置信度）；
2. 改 `internal/codearts/` 对应实现；
3. 更新测试向量（尤其 `signer_test.go`，它是金标准）；
4. 若影响用户可见行为，同步 `README.md` 的「错误语义」与
   [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md)。
