package codearts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// SecretItemKey 是 VS Code secret storage 中存放 CodeArts 登录会话的键。
//
// 注意：secret 挂在 huaweicloud.authentication 扩展名下，而 VS Code 的
// authentication provider id 是 huawei-cloud-authentication —— 两者不同名。
const SecretItemKey = `secret://{"extensionId":"huaweicloud.authentication","key":"HuaweiCloudSession"}`

// Session 是 CodeArts Agent 登录会话中的凭据部分（对应 state.vscdb 里的那条 secret）。
type Session struct {
	AccountLabel  string    `json:"account_label"`
	DomainID      string    `json:"domain_id"`
	AccessKey     string    `json:"access_key"`
	SecretKey     string    `json:"secret_key"`
	SecurityToken string    `json:"security_token"`
	AccessToken   string    `json:"access_token"`
	ExpiresAt     time.Time `json:"expires_at"`
	RefreshToken  string    `json:"refresh_token"`
	// 续期用的 DPoP 密钥与 PKCE verifier（来自 loginContext）
	DPoPPrivateJWK json.RawMessage `json:"dpop_private_jwk,omitempty"`
	DPoPPublicJWK  json.RawMessage `json:"dpop_public_jwk,omitempty"`
	CodeVerifier   string          `json:"code_verifier,omitempty"`
	// Source 记录该会话从哪来：live=刚读到的 state.vscdb，store=状态文件缓存
	Source string `json:"source,omitempty"`

	// raw 保留原始 session JSON，续期回写时只改动凭据字段。
	raw map[string]any
}

// DefaultCredentialTTL 是拿不到过期时间时的保守有效期估计。
//
// IAM 的 STS 临时凭据默认 2 小时，这里按 1 小时 50 分处理，给时钟偏差留余量。
// 手工录入、浏览器登录、会话里缺 expires_at 时都用它。
const DefaultCredentialTTL = 110 * time.Minute

// Valid 报告凭据是否在 margin 之内仍然有效。
func (s *Session) Valid(margin time.Duration) bool {
	if s == nil || s.AccessKey == "" || s.SecretKey == "" {
		return false
	}
	if s.ExpiresAt.IsZero() {
		// 没有过期时间就无从判断新鲜度，按「不可用」处理，
		// 让 Manager 去续期或提示重新登录。
		//
		// 曾经这里直接 return true，等于「永不过期」—— 凭据实际已失效时
		// 网关会一直拿着死凭据打上游，永远不触发续期。
		return false
	}
	return time.Now().Add(margin).Before(s.ExpiresAt)
}

// ExpiresIn 返回距过期的剩余时长。
func (s *Session) ExpiresIn() time.Duration {
	if s == nil || s.ExpiresAt.IsZero() {
		return 0
	}
	return time.Until(s.ExpiresAt)
}

// HasRefresh 报告是否具备自行续期的条件。
func (s *Session) HasRefresh() bool {
	return s != nil && s.RefreshToken != "" && len(s.DPoPPrivateJWK) > 0 && s.CodeVerifier != ""
}

// UpstreamSessionID 返回该凭据在上游的稳定会话 ID。
//
// 必须**复用**：上游按 x-ot-session-id 统计并发会话（每账号上限 3 个），
// 每个请求都换新 ID 会迅速打满上限并返回 TM.00001041（网关侧表现为 429/502）。
// 凭据轮换（AccessKey 变化）后自然换一个新 ID。
//
// 纯函数、不做缓存 —— Session 会被多个 goroutine 共享，写缓存字段就是数据竞争。
func (s *Session) UpstreamSessionID() string {
	if s == nil {
		return ""
	}
	key := s.DomainID + "|" + s.AccessKey
	if key == "|" {
		key = s.AccountLabel
	}
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "ses_" + hex.EncodeToString(sum[:8])
}

// RawJSON 返回可用于回写 state.vscdb / 导出的 session JSON。
//
// 只覆盖凭据相关字段，其余（loginContext / account / domainId …）原样保留，
// 避免把 IDE 侧依赖的字段抹掉。
//
// 对「浏览器登录」创建的会话（没有原始 raw），会把 DPoP 私钥与 PKCE verifier
// 补进 loginContext，这样导出的 JSON 在别处导入后**仍能自动续期**。
func (s *Session) RawJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range s.raw {
		m[k] = v
	}
	m["accessKey"] = s.AccessKey
	m["secretKey"] = s.SecretKey
	m["securitytoken"] = s.SecurityToken
	m["accessToken"] = s.AccessToken
	m["refresh_token"] = s.RefreshToken
	if !s.ExpiresAt.IsZero() {
		m["expires_at"] = s.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if len(s.DPoPPrivateJWK) > 0 {
		s.ensureLoginContext(m)
	}
	return json.Marshal(m)
}

// ensureLoginContext 确保 m 里的 loginContext 带着续期所需的材料。
//
// 只在缺失时补齐 —— 已存在的字段一律不动，否则会改变序列化后的长度，
// 影响 writeBackSession 的「同长度原地覆写」。
//
// m 里的嵌套 map 与 s.raw 共享，所以这里先浅拷贝再补字段：直接改会污染
// 原始会话，并发导出 / 回写时就是数据竞争。
func (s *Session) ensureLoginContext(m map[string]any) {
	lc := map[string]any{}
	for k, v := range nested(m, "loginContext") {
		lc[k] = v
	}
	if _, ok := lc["pkcePair"]; !ok && s.CodeVerifier != "" {
		lc["pkcePair"] = map[string]any{"codeVerifier": s.CodeVerifier}
	}
	pair := map[string]any{}
	for k, v := range nested(lc, "dpopKeyPair") {
		pair[k] = v
	}
	if _, ok := pair["privateKeyJwk"]; !ok {
		pair["privateKeyJwk"] = json.RawMessage(s.DPoPPrivateJWK)
	}
	if _, ok := pair["publicKeyJwk"]; !ok && len(s.DPoPPublicJWK) > 0 {
		pair["publicKeyJwk"] = json.RawMessage(s.DPoPPublicJWK)
	}
	lc["dpopKeyPair"] = pair
	m["loginContext"] = lc
}

// SessionFromJSON 从 state.vscdb 解出的明文 session JSON 构造 Session。
//
// 导出以便在非 Windows 平台（无法自动读取凭据）上，把用户导出的
// session JSON 直接喂给网关。
func SessionFromJSON(plain []byte) (*Session, error) { return parseSession(plain) }

// parseSession 把 state.vscdb 里解出的明文 JSON 解析为 Session。
func parseSession(plain []byte) (*Session, error) {
	var m map[string]any
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, err
	}
	s := &Session{raw: m, Source: "live"}
	s.AccountLabel = str(nested(m, "account"), "label")
	s.DomainID = str(m, "domainId")
	s.AccessKey = str(m, "accessKey")
	s.SecretKey = str(m, "secretKey")
	s.SecurityToken = str(m, "securitytoken")
	s.AccessToken = str(m, "accessToken")
	s.RefreshToken = str(m, "refresh_token")
	if v := str(m, "expires_at"); v != "" {
		if t, err := parseTime(v); err == nil {
			s.ExpiresAt = t
		}
	}
	if s.ExpiresAt.IsZero() {
		// 会话里没有（或无法解析）过期时间：按 STS 常规有效期保守估算，
		// 这样 Valid() 不会把「无从判断」当成「永不过期」。
		s.ExpiresAt = time.Now().Add(DefaultCredentialTTL)
	}
	if lc, ok := m["loginContext"].(map[string]any); ok {
		s.CodeVerifier = str(nested(lc, "pkcePair"), "codeVerifier")
		if pair, ok := lc["dpopKeyPair"].(map[string]any); ok {
			s.DPoPPrivateJWK = marshalRaw(pair["privateKeyJwk"])
			s.DPoPPublicJWK = marshalRaw(pair["publicKeyJwk"])
		}
	}
	return s, nil
}

// str 从 map 中取字符串字段。
func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// nested 取嵌套 map 字段。
func nested(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

// marshalRaw 把任意值重新序列化为 RawMessage。
func marshalRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// parseTime 解析 CodeArts 会话里的时间戳（RFC3339 毫秒）。
func parseTime(v string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &time.ParseError{Layout: time.RFC3339, Value: v}
}

// Account 是网关侧的一个 CodeArts 账号。
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"`    // desktop | manual
	UserData string `json:"user_data"` // %APPDATA%\codearts-agent
	Enabled  bool   `json:"enabled"`
	Status   string `json:"status"`
	Failures int    `json:"failures"`
	LastErr  string `json:"last_err,omitempty"`
	LastUsed int64  `json:"last_used"`
	Note     string `json:"note,omitempty"`
	// 每日免费福利 Token 的签到记录（按本地日期去重）
	LastClaimDate string `json:"last_claim_date,omitempty"`
	LastClaimAt   int64  `json:"last_claim_at,omitempty"`
	LastClaimErr  string `json:"last_claim_err,omitempty"`
	LastClaimMsg  string `json:"last_claim_msg,omitempty"`
	// 缓存的凭据（含 refresh_token 与 DPoP 私钥），用于 IDE 未运行时的续期
	Session *Session `json:"session,omitempty"`
}

// DisplayName 返回账号展示名。
func (a *Account) DisplayName() string {
	if a.Name != "" {
		return a.Name
	}
	return shortID(a.ID)
}

// ImportResult 是一次桌面端导入的结果。
type ImportResult struct {
	Account *Account
	Dir     string
	Profile string
	AK      string
	Expires time.Time
}
