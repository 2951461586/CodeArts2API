package codearts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 续期流程（复刻 extensions/huaweicloud.authentication/dist/plugin.js）：
//
//	POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens
//	DPoP: <ES256 JWS，typ=dpop+jwt，jwk=登录时绑定的公钥>
//	Content-Type: application/x-www-form-urlencoded
//
//	client_id=codearts-agent
//	&code_verifier=<pkcePair.codeVerifier>
//	&grant_type=refresh_token
//	&refresh_token=<refresh_token>
//
// 响应：{"credentials":{"access_key_id","secret_access_key","security_token","expiration"},
//        "refresh_token":"..."}
//
// DPoP 载荷：{"htm":"POST","htu":<token URL>,"iat":<unix>,"jti":<64 位 hex>}

// dpopProof 生成 DPoP 证明（ES256 紧凑 JWS）。
//
// publicJWK 允许为空：EC 私钥 JWK 本身就带 x/y，公钥可以从私钥派生。
// 手工构造的 session JSON（例如从旧版 auths/*.json 转换而来）常常只写了
// privateKeyJwk，缺公钥时不能直接判失败——那会把一次可自愈的导入变成
// 「账号登录态已失效」。
func dpopProof(privateJWK, publicJWK json.RawMessage, htm, htu string) (string, error) {
	key, err := parseECPrivateJWK(privateJWK)
	if err != nil {
		return "", err
	}

	// protected header 必须原样带上登录时绑定的公钥（服务端按 jkt 校验）。
	// 缺失时用私钥里的 x/y 重建——指纹与登录时绑定的仍是同一个。
	pub, err := publicJWKMap(privateJWK, publicJWK)
	if err != nil {
		return "", err
	}
	header := map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": pub}
	payload := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": RandomHex(32),
	}

	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(pb)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64url(sig), nil
}

// RefreshResult 是一次续期的结果。
type RefreshResult struct {
	AccessKey     string
	SecretKey     string
	SecurityToken string
	ExpiresAt     time.Time
	RefreshToken  string
}

// refreshEnvelope 是 IAM 令牌接口的响应（对字段位置保持宽容）。
type refreshEnvelope struct {
	Credentials *struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		Expiration      string `json:"expiration"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
	// 有些网关会把结果再包一层
	Result *struct {
		Credentials *struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
		RefreshToken string `json:"refresh_token"`
	} `json:"result"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Refresh 用 refresh_token 换取新的 STS 临时凭证。
func (c *Client) Refresh(ctx context.Context, sess *Session) (*RefreshResult, error) {
	if !sess.HasRefresh() {
		return nil, fmt.Errorf("缺少 refresh_token / DPoP 私钥 / PKCE verifier，无法续期")
	}
	proof, err := dpopProof(sess.DPoPPrivateJWK, sess.DPoPPublicJWK, http.MethodPost, iamTokenURL)
	if err != nil {
		return nil, err
	}

	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("code_verifier", sess.CodeVerifier)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", sess.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, iamTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("续期请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var env refreshEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("续期响应解析失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	creds := env.Credentials
	refreshToken := env.RefreshToken
	if creds == nil && env.Result != nil {
		creds = env.Result.Credentials
		if refreshToken == "" {
			refreshToken = env.Result.RefreshToken
		}
	}
	if creds == nil || creds.AccessKeyID == "" {
		msg := env.ErrorDescription
		if msg == "" {
			msg = env.Error
		}
		if msg == "" {
			msg = truncate(string(raw), 300)
		}
		return nil, fmt.Errorf("续期失败 HTTP %d: %s", resp.StatusCode, msg)
	}

	out := &RefreshResult{
		AccessKey:     creds.AccessKeyID,
		SecretKey:     creds.SecretAccessKey,
		SecurityToken: creds.SecurityToken,
		RefreshToken:  refreshToken,
	}
	if creds.Expiration != "" {
		if t, err := parseTime(creds.Expiration); err == nil {
			out.ExpiresAt = t
		}
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = time.Now().Add(DefaultCredentialTTL)
	}
	if out.RefreshToken == "" {
		out.RefreshToken = sess.RefreshToken
	}
	return out, nil
}

// --- JWK 处理 ---

// publicJWKMap 返回用于 DPoP protected header 的公钥 JWK。
//
// 优先用显式提供的 publicJWK；为空（或缺少 x/y）时从私钥 JWK 里派生。
// 返回的 map 只含 kty/crv/x/y，绝不带 d。
func publicJWKMap(privateJWK, publicJWK json.RawMessage) (map[string]any, error) {
	if len(publicJWK) > 0 && string(publicJWK) != "null" {
		var pub map[string]any
		if err := json.Unmarshal(publicJWK, &pub); err != nil {
			return nil, fmt.Errorf("DPoP 公钥解析失败: %w", err)
		}
		// 有些导出会把整个私钥塞进 publicKeyJwk，这里必须剥掉 d。
		delete(pub, "d")
		if str(pub, "x") != "" && str(pub, "y") != "" {
			return pub, nil
		}
	}

	var j ecJWK
	if err := json.Unmarshal(privateJWK, &j); err != nil {
		return nil, fmt.Errorf("DPoP 私钥解析失败: %w", err)
	}
	if j.X == "" || j.Y == "" {
		return nil, fmt.Errorf("DPoP 公钥缺失，且无法从私钥派生")
	}
	return map[string]any{"kty": j.Kty, "crv": j.Crv, "x": j.X, "y": j.Y}, nil
}

// ecJWK 是 EC 私钥的 JWK 表示。
type ecJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// parseECPrivateJWK 把 P-256 私钥 JWK 解析为 ecdsa.PrivateKey。
func parseECPrivateJWK(raw json.RawMessage) (*ecdsa.PrivateKey, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("DPoP 私钥缺失")
	}
	var j ecJWK
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("DPoP 私钥解析失败: %w", err)
	}
	if j.Kty != "EC" || j.Crv != "P-256" {
		return nil, fmt.Errorf("DPoP 私钥类型不支持: kty=%s crv=%s", j.Kty, j.Crv)
	}
	x, err := b64urlDecode(j.X)
	if err != nil {
		return nil, fmt.Errorf("DPoP 私钥 x 解析失败: %w", err)
	}
	y, err := b64urlDecode(j.Y)
	if err != nil {
		return nil, fmt.Errorf("DPoP 私钥 y 解析失败: %w", err)
	}
	d, err := b64urlDecode(j.D)
	if err != nil {
		return nil, fmt.Errorf("DPoP 私钥 d 解析失败: %w", err)
	}
	curve := elliptic.P256()
	key := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		},
		D: new(big.Int).SetBytes(d),
	}
	if !curve.IsOnCurve(key.X, key.Y) {
		return nil, fmt.Errorf("DPoP 公钥不在 P-256 曲线上")
	}
	return key, nil
}

// JWKThumbprint 计算 RFC 7638 的 JWK 指纹（与 refresh_token 里的 cnf.jkt 对应）。
func JWKThumbprint(publicJWK json.RawMessage) (string, error) {
	var j ecJWK
	if err := json.Unmarshal(publicJWK, &j); err != nil {
		return "", err
	}
	// 成员必须按字典序、无空白
	canonical := fmt.Sprintf(`{"crv":%q,"kty":%q,"x":%q,"y":%q}`, j.Crv, j.Kty, j.X, j.Y)
	sum := sha256.Sum256([]byte(canonical))
	return b64url(sum[:]), nil
}

// b64url 做无填充的 base64url 编码。
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// b64urlDecode 做无填充的 base64url 解码（兼容带填充的输入）。
func b64urlDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}
