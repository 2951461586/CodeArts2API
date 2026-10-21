package codearts

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDPoPProofDerivesPublicFromPrivate 回归：只给 privateKeyJwk 时必须能签出 DPoP。
//
// 历史 bug：从旧版 auths/*.json 转换 session JSON 时只写了 privateKeyJwk，
// dpopProof 直接 json.Unmarshal(nil) 失败，报「DPoP 公钥解析失败」，再被
// DescribeRefreshError 翻译成「DPoP 密钥校验失败」→ 用户被告知要重新登录，
// 其实凭据完好、公钥能从私钥派生。
func TestDPoPProofDerivesPublicFromPrivate(t *testing.T) {
	pair, err := newDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := dpopProof(pair.PrivateKeyJWK, nil, "POST", IAMTokenURL)
	if err != nil {
		t.Fatalf("缺公钥时应能从私钥派生，却失败: %v", err)
	}
	if !strings.HasPrefix(proof, "eyJ") {
		t.Errorf("不像紧凑 JWS: %q", proof[:20])
	}
	// header 里必须带公钥，且绝不能带私钥 d
	head := strings.Split(proof, ".")[0]
	raw, err := b64urlDecode(head)
	if err != nil {
		t.Fatalf("header 解码失败: %v", err)
	}
	var hdr struct {
		Alg string         `json:"alg"`
		Typ string         `json:"typ"`
		JWK map[string]any `json:"jwk"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		t.Fatalf("header 解析失败: %v", err)
	}
	if hdr.Alg != "ES256" || hdr.Typ != "dpop+jwt" {
		t.Errorf("header = %+v", hdr)
	}
	if _, ok := hdr.JWK["d"]; ok {
		t.Error("DPoP header 里绝不能带私钥 d")
	}
	if hdr.JWK["x"] == nil || hdr.JWK["y"] == nil {
		t.Errorf("header 缺公钥坐标: %+v", hdr.JWK)
	}

	// 派生出的公钥指纹必须与显式给公钥时一致（服务端按 jkt 校验）
	derived := map[string]any{"kty": hdr.JWK["kty"], "crv": hdr.JWK["crv"], "x": hdr.JWK["x"], "y": hdr.JWK["y"]}
	db, _ := json.Marshal(derived)
	want, err := JWKThumbprint(pair.PublicKeyJWK)
	if err != nil {
		t.Fatal(err)
	}
	got, err := JWKThumbprint(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("派生公钥指纹不一致: got %s want %s", got, want)
	}
}

// TestDPoPProofStripsPrivateFieldFromPublicJWK 验证 publicKeyJwk 里混入 d 时被剥掉。
func TestDPoPProofStripsPrivateFieldFromPublicJWK(t *testing.T) {
	pair, err := newDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	// 故意把整个私钥当成公钥传
	proof, err := dpopProof(pair.PrivateKeyJWK, pair.PrivateKeyJWK, "POST", IAMTokenURL)
	if err != nil {
		t.Fatalf("dpopProof: %v", err)
	}
	raw, _ := b64urlDecode(strings.Split(proof, ".")[0])
	var hdr struct {
		JWK map[string]any `json:"jwk"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		t.Fatal(err)
	}
	if _, ok := hdr.JWK["d"]; ok {
		t.Error("publicKeyJwk 里的 d 必须被剥掉")
	}
}

// TestDescribeRefreshErrorLocalDPoPIsNotUpstreamFailure 回归：本地密钥材料缺失
// 不能被翻译成「上游 DPoP 校验失败」，否则会误导用户去重新登录。
func TestDescribeRefreshErrorLocalDPoPIsNotUpstreamFailure(t *testing.T) {
	local := []string{
		"DPoP 公钥解析失败: unexpected end of JSON input",
		"DPoP 私钥缺失",
		"DPoP 公钥缺失，且无法从私钥派生",
	}
	for _, msg := range local {
		got := DescribeRefreshError(errString(msg))
		if got == "DPoP 密钥校验失败" {
			t.Errorf("%q 被误判成上游校验失败", msg)
		}
		if !strings.Contains(got, "本地 DPoP") {
			t.Errorf("%q 的提示 = %q，应指出是本地材料问题", msg, got)
		}
	}

	// 上游真的返回 DPoP 错误时仍走原来那句
	if got := DescribeRefreshError(errString("400 DPoP proof invalid")); got != "DPoP 密钥校验失败" {
		t.Errorf("上游 DPoP 错误提示 = %q", got)
	}
}

// errString 是最小的 error 实现，避免引入 fmt 依赖噪音。
type errString string

func (e errString) Error() string { return string(e) }
