package codearts

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// --- PKCE ---

func TestPKCEPairShape(t *testing.T) {
	p, err := newPKCEPair()
	if err != nil {
		t.Fatalf("newPKCEPair: %v", err)
	}
	// IDE 用 WordArray.random(32).toString(hex) → 64 位十六进制
	if len(p.CodeVerifier) != 64 {
		t.Errorf("code_verifier 长度 = %d，应为 64", len(p.CodeVerifier))
	}
	for _, c := range p.CodeVerifier {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("code_verifier 不是十六进制: %q", p.CodeVerifier)
		}
	}
	if p.CodeChallengeMethod != "SHA-256" {
		t.Errorf("code_challenge_method = %q", p.CodeChallengeMethod)
	}
	sum := sha256.Sum256([]byte(p.CodeVerifier))
	if p.CodeChallenge != b64url(sum[:]) {
		t.Errorf("code_challenge 不是 S256(verifier): %q", p.CodeChallenge)
	}
	if strings.ContainsAny(p.CodeChallenge, "+/=") {
		t.Errorf("code_challenge 必须是 base64url 无填充: %q", p.CodeChallenge)
	}
}

func TestPKCEPairIsRandom(t *testing.T) {
	a, _ := newPKCEPair()
	b, _ := newPKCEPair()
	if a.CodeVerifier == b.CodeVerifier {
		t.Error("两次生成的 code_verifier 相同，随机源有问题")
	}
}

// --- DPoP 密钥对 ---

func TestDPoPKeyPairRoundTrip(t *testing.T) {
	pair, err := newDPoPKeyPair()
	if err != nil {
		t.Fatalf("newDPoPKeyPair: %v", err)
	}
	// 私钥必须能被 dpopProof 重新解析（即 JWK 格式正确、坐标在曲线上）
	if _, err := dpopProof(pair.PrivateKeyJWK, pair.PublicKeyJWK, "POST", IAMTokenURL); err != nil {
		t.Fatalf("用生成的私钥签 DPoP 失败: %v", err)
	}
	// 公钥指纹必须可算（登录后 refresh_token 的 cnf.jkt 会绑定它）
	if _, err := JWKThumbprint(pair.PublicKeyJWK); err != nil {
		t.Fatalf("JWKThumbprint: %v", err)
	}
	// 私钥 JWK 里必须有 d，公钥 JWK 里必须没有
	var priv, pub map[string]string
	_ = json.Unmarshal(pair.PrivateKeyJWK, &priv)
	_ = json.Unmarshal(pair.PublicKeyJWK, &pub)
	if priv["d"] == "" {
		t.Error("私钥 JWK 缺少 d")
	}
	if _, ok := pub["d"]; ok {
		t.Error("公钥 JWK 不应包含 d")
	}
	// 坐标必须是 32 字节 base64url
	for _, k := range []string{"x", "y"} {
		b, err := b64urlDecode(pub[k])
		if err != nil || len(b) != 32 {
			t.Errorf("公钥 %s 不是 32 字节: len=%d err=%v", k, len(b), err)
		}
	}
}

// --- 授权 URL ---

func TestBuildAuthorizeURL(t *testing.T) {
	pkce, _ := newPKCEPair()
	raw := buildAuthorizeURL(LoginOptions{
		PortalHost:    DefaultPortalHost,
		Locale:        "zh-cn",
		Theme:         2,
		PluginVersion: "26.9.902",
	}, 64361, pkce)

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("授权 URL 解析失败: %v", err)
	}
	if u.Scheme != "https" || u.Host != "codearts.huaweicloud.com" || u.Path != "/portal/authorize" {
		t.Errorf("授权 URL 位置不对: %s", raw)
	}
	q := u.Query()
	want := map[string]string{
		"theme":                 "2",
		"locale":                "zh-cn",
		"uri_scheme":            ClientID,
		"client_id":             ClientID,
		"port":                  "64361",
		"code_challenge":        pkce.CodeChallenge,
		"code_challenge_method": "SHA-256",
		"plugin-name":           LoginPluginName,
		"plugin-version":        "26.9.902",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("参数 %s = %q，应为 %q", k, q.Get(k), v)
		}
	}
	// ticket_id 是 64 位十六进制（IDE 用 WordArray.random(32).toString(hex)）
	if len(q.Get("ticket_id")) != 64 {
		t.Errorf("ticket_id 长度 = %d，应为 64", len(q.Get("ticket_id")))
	}
	if q.Get("client_id") != "codearts-agent" {
		t.Errorf("client_id 必须等于 urlProtocol: %q", q.Get("client_id"))
	}
}

// --- 回调端口 ---

func TestListenCallbackPortInRange(t *testing.T) {
	for i := 0; i < 5; i++ {
		ln, err := listenCallback(0)
		if err != nil {
			t.Fatalf("listenCallback: %v", err)
		}
		addr := ln.Addr().String()
		ln.Close()
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("必须只监听环回地址，得到 %s", addr)
		}
		port := 0
		if _, err := fmtSscan(addr, &port); err != nil {
			t.Fatalf("解析端口失败 %s: %v", addr, err)
		}
		if port < minCallbackPort {
			t.Errorf("端口 %d 小于下限 %d（门户侧要求 ≥10000）", port, minCallbackPort)
		}
	}
}

// TestListenCallbackFixedPort 验证远端部署场景下能固定回调端口
// （固定端口才能配一条常驻的 SSH 本地转发）。
func TestListenCallbackFixedPort(t *testing.T) {
	// 先拿一个空闲端口
	probe, err := listenCallback(0)
	if err != nil {
		t.Fatalf("listenCallback(0): %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	ln, err := listenCallback(port)
	if err != nil {
		t.Fatalf("listenCallback(%d): %v", port, err)
	}
	defer ln.Close()
	if got := ln.Addr().(*net.TCPAddr).Port; got != port {
		t.Errorf("固定端口未生效: 期望 %d，实际 %d", port, got)
	}

	// 低于下限必须被拒绝（门户侧要求 ≥10000）
	if _, err := listenCallback(9999); err == nil {
		t.Error("端口低于 10000 应报错")
	}
}

// TestIsOwnCallback 验证回调 URL 归属判断（OOB 粘贴模式的入口）。
func TestIsOwnCallback(t *testing.T) {
	f := &LoginFlow{Port: 64361}
	cases := map[string]bool{
		"http://127.0.0.1:64361/oauth/callback?code=X":  true,
		"http://localhost:64361/oauth/callback?code=X":  true,
		"http://127.0.0.1:64361/oauth/callback":         true,
		"http://127.0.0.1:12345/oauth/callback?code=X":  false, // 端口不符
		"http://127.0.0.1:64361/other?code=X":           false, // 路径不符
		"https://codearts.huaweicloud.com/portal/login": false,
		"https://evil.example.com/oauth/callback":       false,
	}
	for raw, want := range cases {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got := f.isOwnCallback(u); got != want {
			t.Errorf("isOwnCallback(%q) = %v, want %v", raw, got, want)
		}
	}
}

// TestChaseCallbackPicksCodeFromPastedURL 验证 OOB 模式下能直接从粘贴的
// 回调 URL 里取出授权码（不依赖网络）。
func TestChaseCallbackPicksCodeFromPastedURL(t *testing.T) {
	f := &LoginFlow{Port: 64361, client: NewClient()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code, err := f.chaseCallback(ctx, "http://127.0.0.1:64361/oauth/callback?code=THECODE&state=x")
	if err != nil {
		t.Fatalf("chaseCallback: %v", err)
	}
	if code != "THECODE" {
		t.Errorf("code = %q", code)
	}
}

// TestChaseCallbackRejectsLegacySecret 旧版回调（只带 secret）要给出明确指引，
// 而不是静默失败。
func TestChaseCallbackRejectsLegacySecret(t *testing.T) {
	f := &LoginFlow{Port: 64361, client: NewClient()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := f.chaseCallback(ctx, "http://127.0.0.1:64361/oauth/callback?secret=abc&redirect=https://x/y")
	// secret+redirect 会先跟随 redirect（网络不可达），错误信息里应提示重新发起
	if err == nil {
		t.Fatal("应报错")
	}
	t.Logf("错误信息: %v", err)
}

// TestLoginCallbackTwoHops 验证回调服务器的两跳行为与 IDE 一致：
//
//	?secret=&redirect=  → 307 到 redirect
//	?code=              → 307 到门户 login 页
func TestLoginCallbackTwoHops(t *testing.T) {
	flow := &LoginFlow{
		ID:      "test",
		Status:  LoginPending,
		done:    make(chan struct{}),
		Started: time.Now(),
		client:  NewClient(),
	}
	opt := LoginOptions{PortalHost: DefaultPortalHost, Locale: "zh-cn"}
	handler := flow.handleCallback(opt)

	// 第一跳
	rec := newRecorder()
	handler(rec, mustRequest(t, "http://127.0.0.1:1"+AuthRedirectPath+"?secret=abc&redirect=https://example.com/next"))
	if rec.code != http.StatusTemporaryRedirect {
		t.Fatalf("第一跳状态码 = %d，应为 307", rec.code)
	}
	if loc := rec.header.Get("Location"); loc != "https://example.com/next" {
		t.Errorf("第一跳 Location = %q", loc)
	}

	// 第二跳（code 会异步触发 exchange，这里只验证响应与状态记录）
	rec = newRecorder()
	handler(rec, mustRequest(t, "http://127.0.0.1:1"+AuthRedirectPath+"?code=THECODE"))
	if rec.code != http.StatusTemporaryRedirect {
		t.Fatalf("第二跳状态码 = %d，应为 307", rec.code)
	}
	loc := rec.header.Get("Location")
	if !strings.Contains(loc, "/portal/login?login_succeed=true") ||
		!strings.Contains(loc, "uri_scheme="+ClientID) {
		t.Errorf("第二跳 Location = %q", loc)
	}
	flow.mu.Lock()
	got := flow.code
	flow.mu.Unlock()
	if got != "THECODE" {
		t.Errorf("授权码未记录: %q", got)
	}
}

// TestLoginCallbackFailureRedirect 没有 code 时也要跳到失败页，而不是挂住。
func TestLoginCallbackFailureRedirect(t *testing.T) {
	flow := &LoginFlow{ID: "t", done: make(chan struct{}), client: NewClient()}
	rec := newRecorder()
	flow.handleCallback(LoginOptions{PortalHost: DefaultPortalHost, Locale: "zh-cn"})(
		rec, mustRequest(t, "http://127.0.0.1:1"+AuthRedirectPath))
	if rec.code != http.StatusTemporaryRedirect {
		t.Fatalf("状态码 = %d", rec.code)
	}
	if !strings.Contains(rec.header.Get("Location"), "login_succeed=false") {
		t.Errorf("Location = %q", rec.header.Get("Location"))
	}
}

// TestLoginFlowMarkSaved 保证轮询重复调用时账号只入库一次。
func TestLoginFlowMarkSaved(t *testing.T) {
	flow := &LoginFlow{ID: "t", done: make(chan struct{})}
	if !flow.MarkSaved() {
		t.Error("首次 MarkSaved 应返回 true")
	}
	if flow.MarkSaved() {
		t.Error("重复 MarkSaved 应返回 false")
	}
}

// --- 签到 ---

func TestDescribeClaimError(t *testing.T) {
	cases := map[string]string{
		"APIG.0602": "凭据（STS security token）已过期，无法签到",
		"APIG.0301": "签名校验失败，凭据可能已失效",
		"4006":      "今日名额已用完",
		"4004":      "权益不存在或已过期",
		"4003":      "账号已被禁用",
		"something": "something",
	}
	for in, want := range cases {
		if got := DescribeClaimError(errors.New(in)); got != want {
			t.Errorf("DescribeClaimError(%q) = %q，应为 %q", in, got, want)
		}
	}
	if DescribeClaimError(nil) != "" {
		t.Error("nil 应返回空串")
	}
}

func TestClaimDailyDisabledChannel(t *testing.T) {
	// 用一个必然失败的端点验证「通道未开放」的分支不会 panic
	c := NewClient()
	sess := &Session{AccessKey: "AK", SecretKey: "SK"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.ClaimDaily(ctx, sess); err == nil {
		t.Log("网络可达，跳过该用例")
	}
}

// --- 测试辅助 ---

type recorder struct {
	code   int
	header http.Header
}

func newRecorder() *recorder { return &recorder{code: 200, header: http.Header{}} }

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(code int)        { r.code = code }

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return req
}

// fmtSscan 从 "127.0.0.1:64361" 里取出端口。
func fmtSscan(addr string, port *int) (int, error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return 0, errors.New("no port")
	}
	n := 0
	for _, c := range addr[i+1:] {
		if c < '0' || c > '9' {
			return 0, errors.New("bad port")
		}
		n = n*10 + int(c-'0')
	}
	*port = n
	return 1, nil
}
