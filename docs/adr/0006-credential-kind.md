---
id: adr-0006
type: architecture-design
title: "ADR 0006：凭据形态显式建模（STS 临时 / IAM 永久 AK·SK）"
status: active
parent: architecture
---

# ADR 0006：凭据形态显式建模（STS 临时 / IAM 永久 AK·SK）

- 状态：已采纳
- 日期：2026-10

## 背景

上游支持两种凭据形态，生命周期完全不同：

| 形态 | 有效期 | 续期材料 | 签名 |
| --- | --- | --- | --- |
| STS 临时凭据 | 约 2 小时 | refresh_token + DPoP 私钥 + PKCE verifier | SDK-HMAC-SHA256 + `x-security-token` |
| IAM 永久 AK/SK | 不过期 | **没有**（也无续期概念） | 纯 SDK-HMAC-SHA256 |

早期实现只按 STS 建模，隐含假设「凭据一定有过期时间、一定需要续期」：

```go
// ManualAccount：未给过期时间就按 110 分钟推定
if expires.IsZero() {
    expires = time.Now().Add(DefaultCredentialTTL)
}

// Session.Valid：零值过期时间一律判为不可用
if s.ExpiresAt.IsZero() {
    return false
}
```

而 IAM 永久 AK/SK 的过期时间**正是零值**。于是手工录入永久凭据后：

1. `ManualAccount` 给它塞了个 110 分钟后的过期时间；
2. 110 分钟后 `Valid()` 判为过期；
3. `HasRefresh()` 为 false（没有 refresh_token），Manager 无法续期；
4. 账号永久失效，控制台显示 `expires_at` 为 1970（零值时间戳），`expires_in` 为负数。

## 决策

**用显式的 `Session.CredentialKind` 区分两种形态，不靠「有没有过期时间」猜。**

- `kindSTS`（`""`，零值）—— 兼容既有状态文件与所有 OAuth 路径
  （桌面端导入 / 浏览器登录 / session JSON 导入）；
- `kindAKSK`（`"aksk"`）—— 由 `ManualAKSKAccount()` 构造，`SecurityToken` 留空、
  `ExpiresAt` 保持零值。

三个方法的语义按形态分别定义，而不是各自打补丁：

| 方法 | STS | 永久 AK/SK |
| --- | --- | --- |
| `Valid(margin)` | 按 `ExpiresAt - margin` 判断 | **AK/SK 非空即为 true**（短路，不看时间） |
| `HasRefresh()` | 三件套齐全才为 true | 恒 false —— 语义是「无续期概念」，不是「缺材料」 |
| `ExpiresIn()` | `time.Until(ExpiresAt)` | 恒 0，避免控制台显示负数 |
| `IsPermanent()` | false | true（admin 接口 / 控制台据此渲染「永不过期」） |

签名层**不需要改动**：`signRequest` 在 `securityToken` 为空时本就不注入
`x-security-token` 头，永久 AK/SK 走纯 SDK-HMAC-SHA256。

## 后果

### 好处

- 永久 AK/SK 的失效语义正确：不会在 110 分钟后「凭据好好的但账号被判死」；
- 控制台不再显示 1970 时间戳与负数剩余时长；
- `HasRefresh()` 的两种 false 原因（无续期概念 / 缺续期材料）在上层可以区分，
  Manager 不会为一个永久凭据反复尝试续期；
- 旧状态文件不需要迁移：零值就是 STS。

### 代价

- `Session` 多了一个持久化字段，导出 / 导入 session JSON 时一并往返；
- 新增一种「账号形态」，控制台、admin 接口、探活路径都要意识到它的存在
  （已在 `admin/api/state` 暴露 `permanent` 字段）；
- 仍然**不做自动推断**：AK/SK 与 STS 在字面上无法区分，
  必须由调用方（手工录入表单的下拉框）声明。

### 不做的事

- 不把 `CredentialKind` 扩成通用枚举体系（只有两种形态，加抽象层只会增加间接）；
- 不改签名实现（当前实现已经天然支持无 security token 的情形）；
- 不自动检测凭据类型（无从判断，猜错会让账号在 110 分钟后失效）。
