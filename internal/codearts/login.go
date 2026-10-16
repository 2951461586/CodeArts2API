package codearts

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本地浏览器登录（OAuth 2.0 授权码 + PKCE + DPoP）。
//
// 复刻 extensions/huaweicloud.authentication/dist/plugin.js：
//
//  1. 生成 PKCE 对与 DPoP P-256 密钥对，在 127.0.0.1 上起一个随机端口
//     （≥10000，与 IDE 的取端口策略一致）的回调服务器
//  2. 打开浏览器到
//     ${portalHost}/authorize?theme=&locale=&uri_scheme=codearts-agent
//        &client_id=codearts-agent&port=<port>
//        &code_challenge=<S256>&code_challenge_method=SHA-256
//        &ticket_id=<hex>&plugin-name=snap_AIIDE&plugin-version=<ver>
//  3. 浏览器登录后回调两次：
//       GET /oauth/callback?secret=<..>&redirect=<..>   → 307 跟随 redirect
//       GET /oauth/callback?code=<authorizationCode>    → 拿到授权码
//  4. 用授权码换凭据：
//       POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens
//       DPoP: <ES256 JWS>
//       client_id=codearts-agent&code=<code>&code_verifier=<verifier>
//       &grant_type=authorization_code&redirect_uri=<callback>
//
// 注意：回调地址是 127.0.0.1，因此**浏览器必须与网关在同一台机器上**。
// 容器 / 远程部署请改用「导入桌面端登录态」或「从 session JSON 导入」。

// 登录流程相关常量。
const (
	// DefaultPortalHost 是 OAuth 授权页所在的门户。
	DefaultPortalHost = "https://codearts.huaweicloud.com/portal"
	// LoginPluginName 是登录请求里携带的插件名（与 IDE 一致）。
	LoginPluginName = "snap_AIIDE"
	// AuthRedirectPath 是回调路径。
	AuthRedirectPath = "/oauth/callback"
	// loginTimeout 是一次登录流程的最长等待时间。
	loginTimeout = 5 * time.Minute
	// minCallbackPort 是回调端口下限（门户侧要求 ≥10000）。
	minCallbackPort = 10000
	// IAMHost 是 IAM 身份服务（查询账号信息）。
	IAMHost = "https://sts.cn-north-4.myhuaweicloud.com"
	// CallerIdentityPath 返回当前凭据对应的账号信息。
	CallerIdentityPath = "/v5/caller-identity"
)

// LoginOptions 控制一次登录流程。
type LoginOptions struct {
	// PortalHost 覆盖授权页门户（留空用 DefaultPortalHost）。
	PortalHost string
	// Locale 是授权页语言，如 zh-cn。
	Locale string
	// Theme 是授权页主题：1=浅色 2=深色 3=高对比。
	Theme int
	// PluginVersion 是随请求上报的插件版本。
	PluginVersion string
	// Timeout 覆盖默认的 5 分钟等待上限。
	Timeout time.Duration
	// OpenURL 是可选的浏览器打开回调（默认由调用方展示链接）。
	OpenURL func(string)
}

// LoginStatus 是一次登录流程的状态。
type LoginStatus string

// 登录状态取值。
const (
	LoginPending   LoginStatus = "pending"   // 等待用户在浏览器里完成登录
	LoginConfirmed LoginStatus = "confirmed" // 已拿到凭据
	LoginFailed    LoginStatus = "failed"    // 失败
	LoginExpired   LoginStatus = "expired"   // 超时
	LoginCanceled  LoginStatus = "canceled"  // 被取消
)

// LoginFlow 是一次进行中的登录流程。
type LoginFlow struct {
	ID       string
	AuthURL  string
	Port     int
	Status   LoginStatus
	Message  string
	Account  string
	Session  *Session
	Started  time.Time
	Finished time.Time

	client   *Client
	pkce     pkcePair
	dpop     dpopKeyPair
	server   *http.Server
	listener net.Listener
	code     string
	secret   string
	saved    bool

	mu     sync.Mutex
	done   chan struct{}
	closed bool
}

// pkcePair 是 PKCE 的 verifier / challenge。
type pkcePair struct {
	CodeVerifier        string `json:"codeVerifier"`
	CodeChallenge       string `json:"codeChallenge"`
	CodeChallengeMethod string `json:"codeChallengeMethod"`
}

// dpopKeyPair 是 DPoP 的 EC P-256 密钥对（JWK 形式）。
type dpopKeyPair struct {
	PrivateKeyJWK json.RawMessage `json:"privateKeyJwk"`
	PublicKeyJWK  json.RawMessage `json:"publicKeyJwk"`
	key           *ecdsa.PrivateKey
}

// LoginManager 管理进行中的登录流程。
type LoginManager struct {
	Client *Client

	mu    sync.Mutex
	flows map[string]*LoginFlow
}

// NewLoginManager 构造登录管理器。
func NewLoginManager(c *Client) *LoginManager {
	return &LoginManager{Client: c, flows: map[string]*LoginFlow{}}
}

// Start 启动一次登录流程：起本地回调服务器并返回待打开的授权 URL。
func (m *LoginManager) Start(opt LoginOptions) (*LoginFlow, error) {
	if opt.PortalHost == "" {
		opt.PortalHost = DefaultPortalHost
	}
	if opt.Locale == "" {
		opt.Locale = "zh-cn"
	}
	if opt.Theme == 0 {
		opt.Theme = 2 // 深色
	}
	if opt.PluginVersion == "" {
		opt.PluginVersion = PluginVersion
	}
	if opt.Timeout <= 0 {
		opt.Timeout = loginTimeout
	}

	pkce, err := newPKCEPair()
	if err != nil {
		return nil, err
	}
	dpop, err := newDPoPKeyPair()
	if err != nil {
		return nil, err
	}

	ln, err := listenCallback()
	if err != nil {
		return nil, fmt.Errorf("无法在 127.0.0.1 上启动回调服务器: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	flow := &LoginFlow{
		ID:       RandomHex(8),
		client:   m.Client,
		Port:     port,
		Status:   LoginPending,
		Started:  time.Now(),
		pkce:     *pkce,
		dpop:     *dpop,
		listener: ln,
		done:     make(chan struct{}),
	}
	flow.AuthURL = buildAuthorizeURL(opt, port, pkce)

	mux := http.NewServeMux()
	mux.HandleFunc(AuthRedirectPath, flow.handleCallback(opt))
	flow.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = flow.server.Serve(ln) }()
	go flow.watchdog(opt.Timeout)

	if opt.OpenURL != nil {
		opt.OpenURL(flow.AuthURL)
	}

	m.mu.Lock()
	// 顺手清理过期的流程，避免内存里堆积
	for id, f := range m.flows {
		if !f.Finished.IsZero() && time.Since(f.Finished) > 10*time.Minute {
			delete(m.flows, id)
		}
	}
	m.flows[flow.ID] = flow
	m.mu.Unlock()

	return flow, nil
}

// Get 按 ID 取回登录流程。
func (m *LoginManager) Get(id string) *LoginFlow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flows[id]
}

// Cancel 取消一次登录流程。
func (m *LoginManager) Cancel(id string) bool {
	f := m.Get(id)
	if f == nil {
		return false
	}
	f.finish(LoginCanceled, "已取消", nil)
	return true
}

// buildAuthorizeURL 拼出授权页地址。
func buildAuthorizeURL(opt LoginOptions, port int, pkce *pkcePair) string {
	q := url.Values{}
	q.Set("theme", strconv.Itoa(opt.Theme))
	q.Set("locale", opt.Locale)
	q.Set("uri_scheme", ClientID)
	q.Set("client_id", ClientID)
	q.Set("port", strconv.Itoa(port))
	q.Set("code_challenge", pkce.CodeChallenge)
	q.Set("code_challenge_method", pkce.CodeChallengeMethod)
	q.Set("ticket_id", RandomHex(32))
	q.Set("plugin-name", LoginPluginName)
	q.Set("plugin-version", opt.PluginVersion)
	return strings.TrimRight(opt.PortalHost, "/") + "/authorize?" + q.Encode()
}

// handleCallback 处理门户的回调。
//
// 与 IDE 一致：先收 ?secret=&redirect= 这一跳（307 跟随），再收 ?code=。
func (f *LoginFlow) handleCallback(opt LoginOptions) http.HandlerFunc {
	loginOK := strings.TrimRight(opt.PortalHost, "/") +
		"/login?login_succeed=true&uri_scheme=" + ClientID + "&locale=" + url.QueryEscape(opt.Locale)
	loginFail := strings.TrimRight(opt.PortalHost, "/") +
		"/login?login_succeed=false&uri_scheme=" + ClientID + "&locale=" + url.QueryEscape(opt.Locale)

	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// 第一跳：门户带 secret + redirect，我们只负责跳到 redirect
		if secret := q.Get("secret"); secret != "" {
			f.mu.Lock()
			f.secret = secret
			f.mu.Unlock()
			target := q.Get("redirect")
			if target == "" {
				target = loginFail
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			return
		}
		// 第二跳：拿到授权码
		code := q.Get("code")
		if code == "" {
			http.Redirect(w, r, loginFail, http.StatusTemporaryRedirect)
			return
		}
		f.mu.Lock()
		f.code = code
		f.mu.Unlock()
		http.Redirect(w, r, loginOK, http.StatusTemporaryRedirect)

		// 回调线程里完成换取凭据，避免用户浏览器长时间等待
		go f.exchange(code)
	}
}

// watchdog 到点未完成则判超时。
func (f *LoginFlow) watchdog(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-f.done:
	case <-timer.C:
		f.finish(LoginExpired, "登录超时（5 分钟内未完成授权）", nil)
	}
}

// exchange 用授权码换取 STS 凭据。
func (f *LoginFlow) exchange(code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := f.client.ExchangeAuthorizationCode(ctx, code, f.pkce, f.dpop, f.Port)
	if err != nil {
		f.finish(LoginFailed, "换取凭据失败: "+err.Error(), nil)
		return
	}
	f.finish(LoginConfirmed, "登录成功", sess)
}

// MarkSaved 原子地标记「已把账号写入账号池」，返回本次调用是否是第一次。
//
// 轮询接口可能被前端重复调用，用它保证账号只入库一次。
func (f *LoginFlow) MarkSaved() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saved {
		return false
	}
	f.saved = true
	return true
}

// Snapshot 返回流程当前状态的快照。
func (f *LoginFlow) Snapshot() (LoginStatus, string, string, *Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Status, f.Message, f.Account, f.Session
}

// finish 结束流程并释放资源。
func (f *LoginFlow) finish(status LoginStatus, msg string, sess *Session) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.Status = status
	f.Message = msg
	f.Finished = time.Now()
	if sess != nil {
		f.Session = sess
		f.Account = sess.AccountLabel
	}
	f.mu.Unlock()

	close(f.done)
	if f.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = f.server.Shutdown(ctx)
		cancel()
	}
}

// --- 授权码换凭据 ---

// ExchangeAuthorizationCode 用授权码换取 STS 临时凭据。
func (c *Client) ExchangeAuthorizationCode(ctx context.Context, code string, pkce pkcePair, dpop dpopKeyPair, port int) (*Session, error) {
	proof, err := dpopProof(dpop.PrivateKeyJWK, dpop.PublicKeyJWK, http.MethodPost, IAMTokenURL)
	if err != nil {
		return nil, err
	}
	redirect := "http://127.0.0.1:" + strconv.Itoa(port) + AuthRedirectPath

	form := url.Values{}
	form.Set("client_id", ClientID)
	form.Set("code", code)
	form.Set("code_verifier", pkce.CodeVerifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirect)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, IAMTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("换取凭据请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var env refreshEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("换取凭据响应解析失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
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
		return nil, fmt.Errorf("换取凭据失败 HTTP %d: %s", resp.StatusCode, msg)
	}

	sess := &Session{
		AccessKey:      creds.AccessKeyID,
		SecretKey:      creds.SecretAccessKey,
		SecurityToken:  creds.SecurityToken,
		RefreshToken:   refreshToken,
		DPoPPrivateJWK: dpop.PrivateKeyJWK,
		DPoPPublicJWK:  dpop.PublicKeyJWK,
		CodeVerifier:   pkce.CodeVerifier,
		Source:         "browser-login",
	}
	if creds.Expiration != "" {
		if t, err := parseTime(creds.Expiration); err == nil {
			sess.ExpiresAt = t
		}
	}
	if sess.ExpiresAt.IsZero() {
		sess.ExpiresAt = time.Now().Add(110 * time.Minute)
	}
	// 补全账号信息（best-effort，失败不影响凭据可用性）
	if id, name, domain, err := c.CallerIdentity(ctx, sess); err == nil {
		sess.AccountLabel = name
		sess.DomainID = domain
		_ = id
	}
	if sess.AccountLabel == "" {
		sess.AccountLabel = "CodeArts " + shortID(sess.AccessKey)
	}
	return sess, nil
}

// CallerIdentity 查询当前凭据对应的账号信息。
func (c *Client) CallerIdentity(ctx context.Context, sess *Session) (id, name, domainID string, err error) {
	resp, err := c.do(ctx, sess, http.MethodGet, IAMHost, CallerIdentityPath, nil, c.baseHeaders("", ChannelCredit))
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", "", &UpstreamError{Status: resp.StatusCode, Message: truncate(string(raw), 200)}
	}
	var out struct {
		User struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			DomainID string `json:"domain_id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", "", err
	}
	return out.User.ID, out.User.Name, out.User.DomainID, nil
}

// --- PKCE / DPoP 生成 ---

// newPKCEPair 生成 PKCE 对（verifier 为 64 位十六进制，challenge 为 S256）。
func newPKCEPair() (*pkcePair, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	verifier := fmt.Sprintf("%x", buf)
	sum := sha256.Sum256([]byte(verifier))
	return &pkcePair{
		CodeVerifier:        verifier,
		CodeChallenge:       b64url(sum[:]),
		CodeChallengeMethod: "SHA-256",
	}, nil
}

// newDPoPKeyPair 生成 DPoP 用的 EC P-256 密钥对并导出为 JWK。
func newDPoPKeyPair() (*dpopKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	priv, err := json.Marshal(map[string]string{
		"kty": "EC", "crv": "P-256",
		"x": b64url(pad32(key.X)),
		"y": b64url(pad32(key.Y)),
		"d": b64url(pad32(key.D)),
	})
	if err != nil {
		return nil, err
	}
	pub, err := json.Marshal(map[string]string{
		"kty": "EC", "crv": "P-256",
		"x": b64url(pad32(key.X)),
		"y": b64url(pad32(key.Y)),
	})
	if err != nil {
		return nil, err
	}
	return &dpopKeyPair{PrivateKeyJWK: priv, PublicKeyJWK: pub, key: key}, nil
}

// pad32 把大整数补成 32 字节（P-256 坐标长度）。
func pad32(v *big.Int) []byte {
	out := make([]byte, 32)
	v.FillBytes(out)
	return out
}

// listenCallback 在 127.0.0.1 上监听一个 ≥10000 的端口。
//
// 与 IDE 的策略一致：先随机取；若落到 10000 以下，再显式挑一个
// [10000, 65535) 的随机端口重试一次。
func listenCallback() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		if ln.Addr().(*net.TCPAddr).Port >= minCallbackPort {
			return ln, nil
		}
		_ = ln.Close()
	}
	var lastErr error
	for i := 0; i < 8; i++ {
		port := minCallbackPort + int(randUint32()%55536)
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			return l, nil
		}
		lastErr = err
	}
	if err != nil {
		return nil, err
	}
	return nil, lastErr
}

// randUint32 返回一个随机 uint32。
func randUint32() uint32 {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
