package codearts

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestExportRoundTrip 验证「导出 → 导入」闭环。
//
// 这是导出功能最关键的保证：导出的 session JSON 必须能被 SessionFromJSON
// 原样吃回去，并且**续期材料（refresh_token + DPoP 私钥 + PKCE verifier）
// 仍然完整** —— 否则拿到别的机器上导入后只能活 2 小时。
func TestExportRoundTrip(t *testing.T) {
	orig := loadLocalSession(t)
	if orig == nil {
		t.Skip("本机没有可用的 CodeArts 会话")
	}

	exported, err := orig.RawJSON()
	if err != nil {
		t.Fatalf("RawJSON: %v", err)
	}

	// 必须能重新解析
	back, err := SessionFromJSON(exported)
	if err != nil {
		t.Fatalf("SessionFromJSON: %v", err)
	}

	// 凭据字段逐一比对
	if back.AccessKey != orig.AccessKey {
		t.Errorf("accessKey 丢失: %q vs %q", back.AccessKey, orig.AccessKey)
	}
	if back.SecretKey != orig.SecretKey {
		t.Errorf("secretKey 丢失")
	}
	if back.SecurityToken != orig.SecurityToken {
		t.Errorf("securitytoken 丢失")
	}
	if back.RefreshToken != orig.RefreshToken {
		t.Errorf("refresh_token 丢失")
	}
	if back.AccountLabel != orig.AccountLabel {
		t.Errorf("account.label 丢失: %q vs %q", back.AccountLabel, orig.AccountLabel)
	}
	if back.DomainID != orig.DomainID {
		t.Errorf("domainId 丢失: %q vs %q", back.DomainID, orig.DomainID)
	}
	if !back.ExpiresAt.Equal(orig.ExpiresAt) {
		t.Errorf("expires_at 丢失: %v vs %v", back.ExpiresAt, orig.ExpiresAt)
	}

	// 续期材料必须完整 —— 这是「导出后仍能自动续期」的前提
	if !back.HasRefresh() {
		t.Fatal("导入后无法续期：refresh_token / DPoP 私钥 / PKCE verifier 有丢失")
	}
	if back.CodeVerifier != orig.CodeVerifier {
		t.Errorf("PKCE codeVerifier 丢失")
	}

	// DPoP 公钥指纹必须与原会话一致（也就是与 refresh_token 的 cnf.jkt 绑定一致）
	wantJKT, err := JWKThumbprint(orig.DPoPPublicJWK)
	if err != nil {
		t.Fatalf("原会话指纹: %v", err)
	}
	gotJKT, err := JWKThumbprint(back.DPoPPublicJWK)
	if err != nil {
		t.Fatalf("导入后指纹: %v", err)
	}
	if gotJKT != wantJKT {
		t.Errorf("DPoP 指纹变了: %s → %s", wantJKT, gotJKT)
	}

	// 用导出的私钥必须还能签出合法的 DPoP
	if _, err := dpopProof(back.DPoPPrivateJWK, back.DPoPPublicJWK, "POST", IAMTokenURL); err != nil {
		t.Errorf("导入后用 DPoP 私钥签名失败: %v", err)
	}

	// 二次导出应与首次一致（幂等）
	again, err := back.RawJSON()
	if err != nil {
		t.Fatalf("二次 RawJSON: %v", err)
	}
	var a, b map[string]any
	if err := json.Unmarshal(exported, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(again, &b); err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("二次导出的字段数变了: %d → %d", len(a), len(b))
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			t.Errorf("二次导出丢了字段 %q", k)
		}
	}
}

// TestRawJSONLengthStable 验证 RawJSON 不会因为补 loginContext 而改变长度 ——
// writeBackSession 依赖「同长度原地覆写」。
func TestRawJSONLengthStable(t *testing.T) {
	sess := loadLocalSession(t)
	if sess == nil {
		t.Skip("本机没有可用的 CodeArts 会话")
	}
	first, err := sess.RawJSON()
	if err != nil {
		t.Fatal(err)
	}
	// 反复调用必须稳定
	for i := 0; i < 3; i++ {
		got, err := sess.RawJSON()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(first) {
			t.Fatalf("第 %d 次长度变了: %d → %d", i+1, len(first), len(got))
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("第 %d 次内容变了", i+1)
		}
	}
}

// TestEnsureLoginContextFillsMissing 验证浏览器登录建的会话（raw 为空）
// 也能导出成可续期的 JSON。
func TestEnsureLoginContextFillsMissing(t *testing.T) {
	pair, err := newDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{
		AccessKey:      "AK",
		SecretKey:      "SK",
		SecurityToken:  "STS",
		RefreshToken:   "RT",
		CodeVerifier:   "VERIFIER",
		DPoPPrivateJWK: pair.PrivateKeyJWK,
		DPoPPublicJWK:  pair.PublicKeyJWK,
	}
	raw, err := sess.RawJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := SessionFromJSON(raw)
	if err != nil {
		t.Fatalf("导出的 JSON 无法解析: %v", err)
	}
	if !back.HasRefresh() {
		t.Fatal("浏览器登录建的会话导出后应仍可续期")
	}
	if back.CodeVerifier != "VERIFIER" {
		t.Errorf("codeVerifier = %q", back.CodeVerifier)
	}
	if back.RefreshToken != "RT" {
		t.Errorf("refresh_token = %q", back.RefreshToken)
	}
	// DPoP 私钥必须能签
	if _, err := dpopProof(back.DPoPPrivateJWK, back.DPoPPublicJWK, "POST", IAMTokenURL); err != nil {
		t.Errorf("DPoP 私钥丢失: %v", err)
	}
}

// TestEnsureLoginContextDoesNotOverwrite 验证已存在的字段不被改动
// （否则会改变序列化长度，破坏同长度回写）。
func TestEnsureLoginContextDoesNotOverwrite(t *testing.T) {
	raw := []byte(`{"accessKey":"AK","loginContext":{"pkcePair":{"codeVerifier":"ORIGINAL"},` +
		`"dpopKeyPair":{"privateKeyJwk":{"kty":"EC","crv":"P-256","x":"AA","y":"BB","d":"CC"},` +
		`"publicKeyJwk":{"kty":"EC","crv":"P-256","x":"AA","y":"BB"}}}}`)
	sess, err := SessionFromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 故意塞一个不同的私钥，验证不会被写进去
	other, _ := newDPoPKeyPair()
	sess.DPoPPrivateJWK = other.PrivateKeyJWK
	sess.CodeVerifier = "CHANGED"

	out, err := sess.RawJSON()
	if err != nil {
		t.Fatal(err)
	}
	back, err := SessionFromJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	if back.CodeVerifier != "ORIGINAL" {
		t.Errorf("已存在的 codeVerifier 被覆盖: %q", back.CodeVerifier)
	}
	if string(back.DPoPPrivateJWK) == string(other.PrivateKeyJWK) {
		t.Error("已存在的 privateKeyJwk 被覆盖")
	}
}
