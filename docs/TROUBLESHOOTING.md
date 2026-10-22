# 故障排查

按「症状 → 原因 → 处置」组织。找不到答案时，先看[收集诊断信息](#收集诊断信息)一节。

---

## 一、启动与凭据

### 启动日志：`未自动导入桌面端登录态（可在控制台手动导入）: ...`

**原因**：网关没能在 `%APPDATA%\codearts-agent` 找到可解密的登录会话。

| 细分信息 | 原因 | 处置 |
| --- | --- | --- |
| `state.vscdb 中未找到 CodeArts 登录会话（请先在 IDE 里登录）` | 桌面端从未登录，或登录态已被清理 | 打开 CodeArts Agent 并完成登录，再重启网关 |
| `Local State 解析失败` / DPAPI 相关错误 | `Local State` 被其他进程独占，或换了 Windows 用户 | 关闭 IDE 后重试；确认网关与 IDE 是**同一 Windows 用户** |
| `当前平台不支持自动读取 CodeArts Agent 凭据（仅 Windows 支持 DPAPI）` | Linux / macOS / 容器 | 用「从 session JSON 导入」或「手工录入」，见下 |
| 路径不对（自定义安装目录） | User Data 不在默认位置 | 启动时加 `-user-data "D:\path\to\User Data"`，或在控制台「设置 → User Data 目录」填写 |

### 非 Windows / Docker 怎么拿凭据？

DPAPI 只有 Windows 有，容器与 Linux 无法自动读取。三条路：

```bash
# A. 在 Windows 上导出 session JSON，再导入到别处（推荐，仍可自动续期）
codearts2api.exe -export-session session.json
# 或在控制台「账号 → 导出」

# B. 控制台「账号 → 从 session JSON 导入」粘贴
# C. 控制台「账号 → 手工录入」填 AK / SK / STS（不能自动续期）
```

> ⚠️ session JSON 含 `refresh_token`（约 30 天）与 DPoP 私钥，等同于账号登录态。
> 不要提交到版本库、不要贴到聊天里。

### `保存状态文件失败` / `打开状态文件失败`

- 状态文件默认在**可执行文件同目录**（`codearts2api-data.json`）。放到只读目录会失败。
- 用 `-data /path/to/state.json` 指到可写目录（Docker 里是 `/data`，需挂载卷）。
- Windows 上 `0600` 不映射到 ACL —— 权限控制依赖目录 ACL，请勿把状态文件放在共享目录。

---

## 二、鉴权

### 控制台提示 `需要管理密码`（HTTP 401）

设置了管理密码就必须携带。在右上角输入密码后点「记住」（仅当前标签页有效）。
密码存在 `sessionStorage`，关闭标签页即失效；历史版本写在 `localStorage` 的明文会在启动时被清除。

### 控制台提示 `管理接口拒绝访问`（HTTP 403）

两种情形：

1. **未设管理密码**，且请求不是「本机直连」。
   启动日志会提示：`未设置管理密码，管理接口仅允许本机直连访问`。
   → 本机使用即可；远程/容器场景必须设密码。
2. **监听 `0.0.0.0` 且未设密码**。
   启动日志会警告：`⚠️ 监听 ... 且未设置管理密码：管理接口将拒绝一切非本机直连请求`。
   → 加 `-admin-password` 或设环境变量 `CODEARTS_ADMIN_PASSWORD`。

> 「本机直连」= `RemoteAddr` 是环回地址 **且** 请求不带任何代理头
> （`X-Forwarded-For` / `X-Real-Ip` / `Forwarded` / `X-Forwarded-Proto` …）。
> 同机反向代理会把 `RemoteAddr` 变成 `127.0.0.1`，因此带代理头的请求一律按「非本机」处理。

### Docker 里打不开控制台 / 一直 403

容器内看到的来源是 docker 网桥地址，不是环回。**必须**设置密码：

```bash
docker compose run -e CODEARTS_ADMIN_PASSWORD='your-strong-password' codearts2api
# 或写进 .env：CODEARTS_ADMIN_PASSWORD=...
```

### `/v1/*` 返回 401

已签发 API 密钥时必须带 `Authorization: Bearer sk-ca-...`。
未签发密钥时 `/v1/*` 仅允许本机直连；要放开用「设置 → 允许匿名调用」
（`CODEARTS_ALLOW_ANONYMOUS=1`），但这等于把账号额度开放给能访问该端口的任何人。

---

## 三、请求失败

网关会尽量保留语义，按 `error.type` 判断即可：

| 症状 | 含义 | 处置 |
| --- | --- | --- |
| `429` + `rate_limit_error` | 上游并发会话打满（每账号上限 3）或限流 | 降低并发；多账号会更好。网关已对 `TM.00001041` 做过退避重试 |
| `502` + `credentials_error`（Anthropic: `authentication_error`） | **登录态失效**，且无法自动续期 | 重新登录 CodeArts Agent，或用「浏览器登录」/「session JSON 导入」 |
| `400` + `invalid_request_error` | 参数或模型名不合法（含上游 `200 + error_code` 的业务错误） | 检查请求体；上游的 `details[].error_msg` 会原样带回来 |
| `404` | 模型不存在 | `GET /v1/models` 看可用清单 |
| `502` + `upstream_error` | 上游 5xx / 网络错误 | 稍后重试；检查出网与代理 |
| `503` + `server_error` | 账号池为空 | 控制台导入账号 |
| `406` / `The model not equals requestHeader's Model-Id` | 模型名与请求头不一致（常见于别名里带空格或大小写不同） | 别名值必须与上游模型名**完全一致** |

### `404` + 模型名不存在

网关在**转发之前**就拒绝了：该模型名既不在上游清单里，也不是已知别名。
免费权益通道下线后（2026-10）所有模型都真实扣积分，因此网关不再像以前那样
回落到某个默认模型 —— 错误信息里会列出当前可用模型。

处置：`GET /v1/models` 看清单，或检查别名是否还指向已下线的模型
（`glm-5.3-flash` / `deepseek-v4-pro-0813` / `deepseek-v4-flash-0731` 都已不存在）。

### `4005: unsupported model`

模型名出现在网关清单里、但上游不认。常见于「上游刚下线某个模型，而本地缓存还没刷新」：
`POST /admin/api/models/refresh` 或等 10 分钟的自动刷新。

若控制台把「允许真实消耗积分」关掉了，所有模型调用会返回 `403`（不是 `4005`）。

### 账号显示 `error`，但凭据其实是好的

看 `last_err`：

| `last_err` 长什么样 | 含义 | 处置 |
| --- | --- | --- |
| `上游 HTTP 400: ... param is invalid` | 是**你自己的请求**参数越界，不是账号问题 | 修请求。新版网关已不计账号故障、也不换号重试 |
| `上游 HTTP 200: ...` | 旧版本的行为（状态码被写死成 200） | 升级网关；新版会把真实业务码映射成真实状态码 |
| `本地 DPoP 密钥材料不完整…` | session JSON 里只写了私钥、没写公钥 | **无需重新登录**，用完整 session JSON 重新导入即可（新版会从私钥派生公钥） |
| `DPoP 密钥校验失败` | 上游真的拒了 DPoP proof | 重新登录 |

两个历史坑（已修复，仅供对号入座）：

- **任意客户端发错一次参数，就把健康账号标成 `error` 并累加 `failures`** —— 因为当时把
  「上游业务错误」一律计成账号故障。现在 `400/413/422` 直接回给客户端，不计故障。
- **导入 IAM 永久 AK/SK 后，账号在 110 分钟后永久失效** —— 因为 `Valid()` 把
  「过期时间为零值」判为不可用，而永久凭据的过期时间就是零值。现在按
  `credential_kind` 显式区分（控制台会显示「永不过期 / IAM AK/SK」）。

### `409` + `已有测活任务在运行`

`POST /admin/api/models/probe` 同一时刻只允许一个任务：并发测活会把成本翻倍，
还会打满单账号 3 个并发会话的上限、测出假「不可用」。等当前任务结束后再点。

### 非流式请求 `glm-5.3` 报 400 空 body

上游对 `glm-5.3` 的非流式接口只回一个空 body 的 400。网关已自动降级为
「流式 + 本地聚合」，对客户端仍是非流式语义。若你绕过网关直连上游，需自行处理。

### 流式响应中途报错

上游在流开始后失败时，网关只能以 SSE 错误块收尾：
OpenAI 侧是 `data: {"error":{...}}`，Anthropic 侧是 `event: error`。
客户端应把「已收到部分内容后出现 error 块」视为该次请求失败。

---

## 四、活动福利领取

> 免费福利 Token（每日 1000 万 Tokens）已随 benefit 通道于 2026-10 下线，
> 控制台的额度进度条与「免费福利 Token 签到」一并移除。详见
> [`UPSTREAM.md` §3](UPSTREAM.md) 与 [ADR 0005](adr/0005-remove-benefit-channel.md)。

### 「今日领取」显示未领

- 自动领取默认 00:05 执行；确认「设置 → 每天自动领取运营活动奖励」已勾选。
- 领取接口是幂等的：重复领不会重复发放，失败只记错误、下一轮自动重试。
- 报 `凭据（STS security token）已过期，请重新登录` → 凭据已过期，需重新登录或续期。
- 邀请 / 学生认证类活动**永远不会**自动领：必须由真实事件触发，服务端会拒绝直接领取。

### 续期报 `STS5.1806` / `refresh_token 已被使用过`

`refresh_token` 是**单次使用 + 轮转**的：IDE 与网关同时续期会互相作废。
处置（任选其一）：

- 让网关只读、不续期：启动加 `-no-refresh`，由 IDE 负责续期；
- 或关闭 IDE，让网关独占续期。

对应设置：`auto_refresh` / `write_back_to_ide`。

---

## 五、构建与测试

### Windows 上 `go test -race ./...` 报 `cannot find 'ld'`

`-race` 需要 cgo，Go 调用 `gcc` 链接，而 scoop 的 mingw 只有 `ld.bfd.exe`：

```bash
mkdir -p /tmp/ldshim
cp "$HOME/scoop/apps/mingw/current/bin/ld.bfd.exe" /tmp/ldshim/ld.exe
PATH="/tmp/ldshim:$PATH" go test -race ./...
```

### 控制台「能输密码但点什么都没反应」

脚本有语法错误 → 整段不执行。跑：

```bash
python tools/check_html_js.py
```

它会做跨行字符串检查 + `node --check` + CSS 括号/外部资源检查。
`go test ./internal/server/` 里也有对应守卫（跨行字符串、括号配对、
无内联 style/script、脚本引用的 id 必须存在于 HTML）。

### 页面样式全丢 / 控制台空白但网络 200

CSP 收紧后，任何内联 `<style>` / `<script>` / `style="..."` 都会被浏览器拒绝。
检查是否有人把内联代码加回了 `internal/server/web/index.html`，
`TestConsoleHTMLHasNoInlineCode` 会直接失败。

---

## 六、数据与恢复

### 状态文件损坏 / 想重置

```bash
# 备份后删除，重启会生成空状态（账号与密钥需重新导入/签发）
cp codearts2api-data.json codearts2api-data.json.bak
rm codearts2api-data.json
```

状态文件有 `version` 字段；版本高于本程序时启动会报
`状态文件版本 N 高于本程序支持的 M，请升级 codearts2api` —— 请升级程序，不要删文件。

### 续期回写改坏了 `state.vscdb`

回写只在**长度完全一致**时做定长原地覆写，并在写入前备份：

```text
%APPDATA%\codearts-agent\User\globalStorage\state.vscdb.codearts2api.bak
```

恢复：关闭 IDE → 把 `.bak` 覆盖回 `state.vscdb`。
若不想让网关碰 IDE 数据库，关闭「设置 → 续期后回写 state.vscdb」。

### 想彻底卸载

1. 关闭网关与 IDE；
2. 删除状态文件 `codearts2api-data.json`（含凭据）；
3. 若有 `.codearts2api.bak`，按需恢复或删除；
4. 删除网关签发的 API 密钥（在控制台或直接删状态文件）。

---

## 收集诊断信息

```bash
# 1) 版本
codearts2api -version

# 2) 启动日志（含状态文件路径、导入结果、鉴权模式）
codearts2api -no-import 2>&1 | head -40

# 3) 本机诊断：凭据 / 模型清单 / 活动福利（加 -model glm-5.3 才做对话探活，会消耗少量积分）
go run ./cmd/probe

# 4) 端到端（非流式 / 流式 / 思维链 / 工具调用 / Anthropic）
python tools/local_session_e2e.py
```

> 贴日志前请**脱敏**：`AK`、`X-Security-token`、`Authorization`、
> `refresh_token`、`codearts2api-data.json` 内容都不要外发。
