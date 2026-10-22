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
	// Port 指定回调端口（0 = 随机取一个 ≥10000 的端口）。
	//
	// 远端部署时把它固定下来，就能配一条常驻的 SSH 本地端口转发：
	//     ssh -N -L <Port>:127.0.0.1:<Port> user@server
	// 之后浏览器访问 127.0.0.1:<Port> 会被隧道送到服务器的回调服务器。
	Port int
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

	client *Client
	pkce   pkcePair
	dpop   dpopKeyPair
	server *http.Server
	// portalHostname 是本次登录使用的门户主机名（自建 / 测试环境时
	// 跳转目标可能落在它上面，因此允许跟随）。
	portalHostname string
	listener       net.Listener
	code           string
	secret         string
	saved          bool

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
	// portal_host 来自控制台请求体，而登录流程会把浏览器与 chaseCallback 的
	// 服务端请求都导向它 —— 先校验 scheme，后面 allowedHop 才能信任它。
	if err := validatePortalHost(opt.PortalHost); err != nil {
		return nil, err
	}

	pkce, err := newPKCEPair()
	if err != nil {
		return nil, err
	}
	dpop, err := newDPoPKeyPair()
	if err != nil {
		return nil, err
	}

	ln, err := listenCallback(opt.Port)
	if err != nil {
		return nil, fmt.Errorf("无法在 127.0.0.1 上启动回调服务器: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	flow := &LoginFlow{
		ID:             RandomHex(8),
		client:         m.Client,
		Port:           port,
		Status:         LoginPending,
		Started:        time.Now(),
		pkce:           *pkce,
		dpop:           *dpop,
		portalHostname: hostnameOf(opt.PortalHost),
		listener:       ln,
		done:           make(chan struct{}),
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

// CallbackHint 描述「浏览器打不开回调地址」时该怎么办。
func (f *LoginFlow) CallbackHint() string {
	return fmt.Sprintf("回调地址是 http://127.0.0.1:%d%s —— "+
		"浏览器会把它解析到**你自己的机器**，因此远端部署时要么用 SSH 端口转发， "+
		"要么把浏览器最终停在的那个地址粘贴回控制台（粘贴回调 URL）。", f.Port, AuthRedirectPath)
}

// SubmitCallbackURL 处理用户手工粘贴的回调 URL（远端部署、无隧道时的兜底）。
//
// 用户在自己的浏览器里完成登录后，门户会把浏览器 307 到
// http://127.0.0.1:<port>/oauth/callback?...；如果那台机器上没有监听，
// 浏览器会报「无法访问此网站」，但**地址栏里的 URL 是完整的**。
// 把它粘过来，网关会自己把剩下的重定向链走完并取出授权码。
func (f *LoginFlow) SubmitCallbackURL(raw string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	code, err := f.chaseCallback(ctx, raw)
	if err != nil {
		return "", err
	}
	go f.exchange(code)
	return code, nil
}

// chaseCallback 从给定 URL 出发沿重定向链走到本机回调，取出授权码。
func (f *LoginFlow) chaseCallback(ctx context.Context, raw string) (string, error) {
	cur := strings.TrimSpace(raw)
	if cur == "" {
		return "", fmt.Errorf("回调 URL 为空")
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		// 自己接管重定向，逐跳观察
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for hop := 1; hop <= 10; hop++ {
		u, err := url.Parse(cur)
		if err != nil {
			return "", fmt.Errorf("第 %d 跳 URL 解析失败: %w", hop, err)
		}
		// 命中自己的回调服务器 → 直接读参数，不需要真的发 HTTP
		if f.isOwnCallback(u) {
			q := u.Query()
			if c := q.Get("code"); c != "" {
				return c, nil
			}
			if r := q.Get("redirect"); r != "" {
				cur = r
				continue
			}
			if q.Get("secret") != "" {
				return "", fmt.Errorf("这是旧版登录回调（只带 secret），" +
					"网关只支持新版授权码流程；请在面板重新发起一次浏览器登录")
			}
			return "", fmt.Errorf("回调 URL 里没有 code 参数")
		}
		// 服务端只允许跟随到华为云门户 / 本机回调：redirect 参数来自用户粘贴的
		// 外部输入，不加限制就是一个 SSRF 面。
		if !f.allowedHop(u) {
			return "", fmt.Errorf("第 %d 跳的目标不在允许范围内（%s）：只允许华为云门户与本机回调地址。"+
				"请把浏览器最终停在的那个地址（含 code= 或 redirect= 参数）粘贴过来", hop, u.Host)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "codearts2api/"+PluginVersion)
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("第 %d 跳请求失败: %w", hop, err)
		}
		loc := resp.Header.Get("Location")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if loc == "" {
			return "", fmt.Errorf("重定向链在第 %d 跳中断（HTTP %d，无 Location）。"+
				"请在浏览器里打开授权页后，把**最终停在的那个地址**（含 code= 或 redirect= 参数）粘贴过来",
				hop, resp.StatusCode)
		}
		next, err := u.Parse(loc)
		if err != nil {
			return "", fmt.Errorf("第 %d 跳 Location 解析失败: %w", hop, err)
		}
		cur = next.String()
	}
	return "", fmt.Errorf("重定向次数过多（>10），放弃")
}

// allowedChaseHostSuffixes 是服务端允许跟随跳转的域名后缀。
//
// chaseCallback 会**由服务端**去请求这些地址，而待跟随的 redirect 参数
// 来自用户粘贴的外部输入 —— 不加限制就是一个 SSRF 面。
var allowedChaseHostSuffixes = []string{
	".huaweicloud.com",
	".huaweicloud.cn",
	".huawei.com",
	".myhuaweicloud.com",
	".myhuaweicloud.cn",
}

// allowedHop 报告是否允许服务端跟随到该跳转目标。
//
// 只允许：本机回调地址、本次登录使用的门户主机（已在 Start 校验过）、
// 以及华为云域名（https）。
func (f *LoginFlow) allowedHop(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	if isLoopbackName(host) {
		return true
	}
	if u.Scheme != "https" {
		return false
	}
	// 自建 / 测试环境的门户（scheme 已在 Start 里校验）
	if f.portalHostname != "" && host == f.portalHostname {
		return true
	}
	for _, suffix := range allowedChaseHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// isLoopbackName 判断主机名是否为本机回环。
func isLoopbackName(host string) bool {
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// validatePortalHost 校验授权门户地址。
//
// 只接受 https；本机回环地址额外允许 http（本地联调）。
func validatePortalHost(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("门户地址无效: %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackName(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("门户地址只支持 https（本机回环可用 http）: %q", raw)
}

// hostnameOf 取 URL 的主机名（小写）；解析失败返回空串。
func hostnameOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// isOwnCallback 判断 URL 是否指向本流程的回调服务器。
func (f *LoginFlow) isOwnCallback(u *url.URL) bool {
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return false
	}
	if u.Path != AuthRedirectPath {
		return false
	}
	port := u.Port()
	return port == "" || port == strconv.Itoa(f.Port)
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
	proof, err := dpopProof(dpop.PrivateKeyJWK, dpop.PublicKeyJWK, http.MethodPost, c.tokenURL())
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(form.Encode()))
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
		sess.ExpiresAt = time.Now().Add(DefaultCredentialTTL)
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
	resp, err := c.do(ctx, sess, http.MethodGet, IAMHost, CallerIdentityPath, nil, c.baseHeaders(sess, ""))
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
func listenCallback(fixed int) (net.Listener, error) {
	if fixed > 0 {
		if fixed < minCallbackPort {
			return nil, fmt.Errorf("回调端口 %d 小于下限 %d（门户侧要求 ≥10000）", fixed, minCallbackPort)
		}
		return net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(fixed))
	}
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
