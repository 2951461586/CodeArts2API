package codearts

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- 签名器 ---

func TestCanonicalURIAddsTrailingSlash(t *testing.T) {
	cases := map[string]string{
		"/api/v2/chat/completions":  "/api/v2/chat/completions/",
		"/api/v2/chat/completions/": "/api/v2/chat/completions/",
		"/":                         "/",
		"/a/b":                      "/a/b/",
	}
	for in, want := range cases {
		u, err := url.Parse("https://example.com" + in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		if got := canonicalURI(u); got != want {
			t.Errorf("canonicalURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalURIEncodesLikeHuaweiSigner(t *testing.T) {
	// 华为云 JS signer 只保留 A-Za-z0-9-._~，其余一律 %XX 大写。
	// Go 的 url.PathEscape 会放过 $&+,:;=@，必须自己编码。
	u, _ := url.Parse("https://example.com/a b/c$d,e:f;g=h@i+j")
	got := canonicalURI(u)
	want := "/a%20b/c%24d%2Ce%3Af%3Bg%3Dh%40i%2Bj/"
	if got != want {
		t.Errorf("canonicalURI = %q, want %q", got, want)
	}
}

func TestCanonicalQuerySortedAndEncoded(t *testing.T) {
	u, _ := url.Parse("https://example.com/x?name=xhs-skill-contest-switch&b=2&a=1")
	got := canonicalQuery(u)
	want := "a=1&b=2&name=xhs-skill-contest-switch"
	if got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
}

func TestSignRequestShape(t *testing.T) {
	hdrs, err := signRequest("AKID", "SECRET", "STSTOKEN", "POST",
		"https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions",
		[]byte(`{"a":1}`), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if hdrs["host"] != "snap-access.cn-north-4.myhuaweicloud.com" {
		t.Errorf("host = %q", hdrs["host"])
	}
	if hdrs["x-security-token"] != "STSTOKEN" {
		t.Errorf("x-security-token 缺失")
	}
	auth := hdrs["Authorization"]
	if !strings.HasPrefix(auth, "SDK-HMAC-SHA256 Access=AKID, SignedHeaders=") {
		t.Fatalf("Authorization 前缀不对: %q", auth)
	}
	// 所有参与签名的头都必须出现在 SignedHeaders 里，且按字典序
	sh := strings.Split(strings.Split(strings.Split(auth, "SignedHeaders=")[1], ", Signature=")[0], ";")
	for i := 1; i < len(sh); i++ {
		if sh[i-1] >= sh[i] {
			t.Errorf("SignedHeaders 未按字典序: %v", sh)
		}
	}
	joined := ";" + strings.Join(sh, ";") + ";"
	for _, name := range []string{"host", "x-sdk-date", "x-security-token", "content-type"} {
		if !strings.Contains(joined, ";"+name+";") {
			t.Errorf("SignedHeaders 缺少 %q: %v", name, sh)
		}
	}
}

// TestSignRequestStableVector 锁定签名算法：固定时间戳与固定输入，
// 校验签名值不会因重构而漂移（任何一处 canonical 拼装改动都会让该值变化）。
func TestSignRequestStableVector(t *testing.T) {
	// 通过注入固定 X-Sdk-Date 得到确定性结果
	hdrs, err := signRequest("AK", "SK", "", "GET",
		"https://example.com/a/b?z=1&a=2", nil,
		map[string]string{"X-Sdk-Date": "20260101T000000Z"})
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	// canonical request（按 signer.go 的拼装规则）:
	//   GET\n/a/b/\na=2&z=1\nhost:example.com\nx-sdk-date:20260101T000000Z\n\n
	//   host;x-sdk-date\n<sha256("")>
	canonical := "GET\n" +
		"/a/b/\n" +
		"a=2&z=1\n" +
		"host:example.com\nx-sdk-date:20260101T000000Z\n" +
		"\n" +
		"host;x-sdk-date\n" +
		sha256Hex(nil)
	want := expectedSignature("SK", "20260101T000000Z", canonical)
	if !strings.HasSuffix(hdrs["Authorization"], "Signature="+want) {
		t.Errorf("签名不匹配\n得到: %s\n期望后缀: Signature=%s", hdrs["Authorization"], want)
	}
}

// --- JWK / DPoP ---

func TestJWKThumbprintMatchesRefreshTokenBinding(t *testing.T) {
	sess := loadLocalSession(t)
	if sess == nil || !sess.HasRefresh() {
		t.Skip("本机没有可用的 CodeArts 会话")
	}
	jkt, err := JWKThumbprint(sess.DPoPPublicJWK)
	if err != nil {
		t.Fatalf("JWKThumbprint: %v", err)
	}
	// refresh_token 的 payload 里带 cnf.jkt，必须与本地公钥指纹一致
	want := refreshTokenJKT(t, sess.RefreshToken)
	if want == "" {
		t.Skip("refresh_token 里没有 cnf.jkt")
	}
	if jkt != want {
		t.Errorf("DPoP 指纹不一致: 本地 %s, refresh_token 绑定 %s", jkt, want)
	}
}

func TestDPoPProofVerifies(t *testing.T) {
	sess := loadLocalSession(t)
	if sess == nil || !sess.HasRefresh() {
		t.Skip("本机没有可用的 CodeArts 会话")
	}
	proof, err := dpopProof(sess.DPoPPrivateJWK, sess.DPoPPublicJWK, "POST", IAMTokenURL)
	if err != nil {
		t.Fatalf("dpopProof: %v", err)
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("JWS 分段数不对: %d", len(parts))
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("header 解码失败: %v", err)
	}
	var hdr struct {
		Alg string         `json:"alg"`
		Typ string         `json:"typ"`
		JWK map[string]any `json:"jwk"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		t.Fatalf("header 解析失败: %v", err)
	}
	if hdr.Alg != "ES256" || hdr.Typ != "dpop+jwt" {
		t.Errorf("header 不对: alg=%s typ=%s", hdr.Alg, hdr.Typ)
	}
	if hdr.JWK["d"] != nil {
		t.Errorf("DPoP header 里绝不能带私钥 d")
	}
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var payload struct {
		Htm string `json:"htm"`
		Htu string `json:"htu"`
		Iat int64  `json:"iat"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(pb, &payload); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	if payload.Htm != "POST" || payload.Htu != IAMTokenURL || payload.Iat == 0 || len(payload.Jti) != 64 {
		t.Errorf("payload 不对: %+v", payload)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature 解码失败: %v", err)
	}
	if len(sig) != 64 {
		t.Errorf("ES256 签名长度应为 64，得到 %d", len(sig))
	}
}

// --- SQLite 读取与原地覆写 ---

// TestPatchItemValueRoundTrip 在临时副本上验证「同长度原地覆写」：
// 写入后重新读取必须得到同样的值，且未改动其它行。
func TestPatchItemValueRoundTrip(t *testing.T) {
	dir := localUserData(t)
	if dir == "" {
		t.Skip("本机没有 CodeArts Agent User Data")
	}
	dbPath := filepath.Join(dir, "User", "globalStorage", "state.vscdb")
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("读取 state.vscdb 失败: %v", err)
	}

	// 1) 用同样的值覆写：文件应当逐字节不变（证明偏移映射正确）
	before := append([]byte(nil), raw...)
	db, err := openSQLite(raw)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	old, err := db.ReadItemValue("ItemTable", SecretItemKey)
	if err != nil {
		t.Fatalf("ReadItemValue: %v", err)
	}
	if err := db.PatchItemValue("ItemTable", SecretItemKey, append([]byte(nil), old...)); err != nil {
		t.Fatalf("PatchItemValue: %v", err)
	}
	if !bytes.Equal(before, db.data) {
		t.Errorf("用相同值覆写后文件发生了变化（偏移映射有误）")
	}

	// 2) 写入一个同长度但内容不同的值，必须能读回
	mutated := append([]byte(nil), old...)
	mutated[len(mutated)-2] ^= 0x01
	if err := db.PatchItemValue("ItemTable", SecretItemKey, mutated); err != nil {
		t.Fatalf("PatchItemValue(mutated): %v", err)
	}
	got, err := db.ReadItemValue("ItemTable", SecretItemKey)
	if err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	if !bytes.Equal(got, mutated) {
		t.Errorf("回读值与写入值不一致")
	}

	// 3) 长度不一致必须被拒绝，而不是悄悄写坏
	if err := db.PatchItemValue("ItemTable", SecretItemKey, append(mutated, 'x')); err != ErrLengthMismatch {
		t.Errorf("长度不一致应返回 ErrLengthMismatch，得到 %v", err)
	}
}

// --- 测试辅助 ---

func localUserData(t *testing.T) string {
	t.Helper()
	for _, d := range DefaultUserDataDirs("") {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			if _, err := os.Stat(filepath.Join(d, "User", "globalStorage", "state.vscdb")); err == nil {
				return d
			}
		}
	}
	return ""
}

func loadLocalSession(t *testing.T) *Session {
	t.Helper()
	dir := localUserData(t)
	if dir == "" {
		return nil
	}
	s, err := ReadSession(dir)
	if err != nil {
		t.Logf("ReadSession 失败: %v", err)
		return nil
	}
	return s
}

// refreshTokenJKT 从 refresh_token（JWT）的 payload 里取 cnf.jkt。
func refreshTokenJKT(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var payload struct {
		Cnf struct {
			Jkt string `json:"jkt"`
		} `json:"cnf"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.Cnf.Jkt
}

// copyTree 递归复制目录（测试用）。
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		// 只复制回写测试需要的两个文件，避免复制几百 MB 的缓存
		base := filepath.Base(path)
		if base != "Local State" && base != "state.vscdb" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}

// expectedSignature 独立复算一次签名，用于锁定算法向量。
func expectedSignature(sk, date, canonicalRequest string) string {
	stringToSign := strings.Join([]string{
		sdkSigningAlgorithm, date, sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	return hmacHex(sk, stringToSign)
}

// hmacHex 计算 HMAC-SHA256 的十六进制串。
func hmacHex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
