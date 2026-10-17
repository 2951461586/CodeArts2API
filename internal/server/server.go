// Package server 暴露 OpenAI 兼容的 HTTP 接口。
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// Server 是网关 HTTP 服务。
type Server struct {
	Store  *store.Store
	Up     *codearts.Client
	Cred   *codearts.Manager
	Reg    *registry.Registry
	Logins *codearts.LoginManager

	// 上游模型清单 / 额度的内存缓存
	cache *modelCache
}

// New 构造服务。
func New(st *store.Store) *Server {
	up := codearts.NewClient()
	s := &Server{Store: st, Up: up, Reg: registry.New(), cache: newModelCache()}
	s.Cred = codearts.NewManager(up)
	// 凭据解析拿到的是账号快照，所以续期 / 跟随 IDE 得到的新会话必须显式写回 store，
	// 否则只对那一次请求生效（IDE 关闭后就再也拿不到）。
	s.Cred.Persist = func(a *codearts.Account) error {
		st.SetAccountSession(a.ID, a.Session)
		return st.Save()
	}
	s.Cred.Logf = log.Printf
	s.Logins = codearts.NewLoginManager(up)
	return s
}

// Handler 返回完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 探活
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("pong"))
	})
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)

	// 推理
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChatCompletions))
	mux.HandleFunc("/v1/messages", s.withAuth(s.handleAnthropicMessages))

	// 控制台 API（统一走管理鉴权 + 同源校验）
	mux.HandleFunc("/admin/api/state", s.withAdminAuth(s.handleAdminState))
	mux.HandleFunc("/admin/api/accounts", s.withAdminAuth(s.handleAdminAccounts))
	mux.HandleFunc("/admin/api/keys", s.withAdminAuth(s.handleAdminKeys))
	mux.HandleFunc("/admin/api/logs", s.withAdminAuth(s.handleAdminLogs))
	mux.HandleFunc("/admin/api/stats", s.withAdminAuth(s.handleAdminStats))
	mux.HandleFunc("/admin/api/settings", s.withAdminAuth(s.handleAdminSettings))
	mux.HandleFunc("/admin/api/checkin", s.withAdminAuth(s.handleAdminCheckin))
	mux.HandleFunc("/admin/api/models/refresh", s.withAdminAuth(s.handleAdminRefreshModels))
	mux.HandleFunc("/admin/api/balance", s.withAdminAuth(s.handleAdminBalance))
	mux.HandleFunc("/admin/api/claim", s.withAdminAuth(s.handleAdminClaim))
	mux.HandleFunc("/admin/api/welfare", s.withAdminAuth(s.handleAdminWelfare))
	mux.HandleFunc("/admin/api/invite", s.withAdminAuth(s.handleAdminInvite))
	mux.HandleFunc("/admin/api/login/start", s.withAdminAuth(s.handleAdminLoginStart))
	mux.HandleFunc("/admin/api/login/poll", s.withAdminAuth(s.handleAdminLoginPoll))
	mux.HandleFunc("/admin/api/login/callback", s.withAdminAuth(s.handleAdminLoginCallback))
	mux.HandleFunc("/admin/api/login/cancel", s.withAdminAuth(s.handleAdminLoginCancel))

	// 控制台静态资源（内嵌，同源）
	mux.HandleFunc("/console.css", serveConsoleCSS)
	mux.HandleFunc("/console.js", serveConsoleJS)

	mux.HandleFunc("/", s.handleRoot)

	return s.withCORS(mux)
}

// withCORS 处理跨域。
//
// 默认只放行**同源**请求（即控制台自己的页面）；需要别的来源时在设置里
// 显式列出（逗号分隔）。不提供通配符。
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := s.corsOrigin(r)
		writeCORSHeaders(w, allowed)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeCORSHeaders 仅在来源通过校验时写入跨域响应头。
func writeCORSHeaders(w http.ResponseWriter, allowed string) {
	if allowed == "" || hasWildcard(allowed) {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", allowed)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version, x-admin-password")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
}

// corsOrigin 返回应当回给浏览器的 Allow-Origin 值，空串表示不放行。
//
// 默认只放行**同源**：以前默认放行 localhost / 127.0.0.1 / [::1] 的任意端口，
// 等于让本机随便一个 Web 服务都能读到 /admin/api/* 的响应（含导出登录态）。
// 需要跨来源时在设置里显式列出。
func (s *Server) corsOrigin(r *http.Request) string {
	raw := r.Header.Get("Origin")
	if raw == "" || raw == "null" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	if cfg := strings.TrimSpace(s.Store.Settings().CORSOrigin); cfg != "" {
		for _, entry := range strings.Split(cfg, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && !hasWildcard(entry) && strings.EqualFold(entry, raw) {
				return entry
			}
		}
		return ""
	}
	// 同源（浏览器对同源 POST 也会带 Origin）：回显即可
	if strings.EqualFold(u.Host, r.Host) {
		return raw
	}
	return ""
}

// hasWildcard 报告字符串是否含通配符字符。
func hasWildcard(s string) bool { return strings.IndexByte(s, '*') >= 0 }

// isLoopbackHost 判断主机名是否为本机环回地址。
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// IsLoopbackHost 供启动时判断监听范围（是否只监听本机）。
func IsLoopbackHost(host string) bool { return isLoopbackHost(host) }

// withAuth 校验 Bearer 密钥。
//
// 未签发任何密钥时，为首次使用的便利允许匿名调用 —— 但**仅限本机直连**。
// 否则一旦把监听地址设成 0.0.0.0（Dockerfile 的默认 CMD 就是），
// 任何人都能白嫖账号额度。需要对外匿名开放时，在设置里显式打开
// 「允许匿名调用」（allow_anonymous）。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Store.HasKeys() {
			if s.Store.Settings().AllowAnonymous || s.localDirect(r) {
				next(w, r)
				return
			}
			writeErr(w, http.StatusUnauthorized,
				"尚未签发 API 密钥：请先在控制台创建密钥；若确实需要匿名开放，"+
					"请在设置里打开「允许匿名调用」。",
				"invalid_request_error")
			return
		}
		key := extractKey(r)
		if key == "" {
			writeErr(w, http.StatusUnauthorized, "缺少 API 密钥", "invalid_request_error")
			return
		}
		if !s.Store.ValidKey(key) {
			writeErr(w, http.StatusUnauthorized, "API 密钥无效或已停用", "invalid_request_error")
			return
		}
		next(w, r)
	}
}

// withAdminAuth 保护控制台接口。
//
// 鉴权策略：
//   - 已设置管理密码：必须通过 X-Admin-Password 或 Authorization: Bearer 提供；
//   - 未设置管理密码：仅允许**本机直连**（环回地址且未经代理转发）。
//
// 另外对写操作做同源校验，阻断跨站请求伪造。
func (s *Server) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pw := s.Store.Settings().AdminPassword; pw != "" {
			if !secureEqual(adminPasswordFrom(r), pw) {
				writeErr(w, http.StatusUnauthorized, "管理密码缺失或错误", "invalid_request_error")
				return
			}
		} else if !s.localDirect(r) {
			writeErr(w, http.StatusForbidden,
				"未设置管理密码，管理接口仅允许本机直连访问（检测到代理转发或非环回地址）。"+
					"如通过反向代理 / 容器暴露，请在设置里配置管理密码。",
				"invalid_request_error")
			return
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !s.adminSameOrigin(r) {
				writeErr(w, http.StatusForbidden, "跨站请求被拒绝", "invalid_request_error")
				return
			}
		}
		next(w, r)
	}
}

// adminPasswordFrom 从请求头提取管理密码。
func adminPasswordFrom(r *http.Request) string {
	if v := r.Header.Get("X-Admin-Password"); v != "" {
		return v
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(h)
	}
	return ""
}

// secureEqual 以恒定时间比较两个字符串（先哈希，避免长度泄露）。
func secureEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// adminSameOrigin 校验写操作来源：与请求 Host 同源，或命中显式 CORS 允许列表。
func (s *Server) adminSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if cfg := strings.TrimSpace(s.Store.Settings().CORSOrigin); cfg != "" {
		for _, entry := range strings.Split(cfg, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" && !hasWildcard(entry) && strings.EqualFold(entry, origin) {
				return true
			}
		}
	}
	return false
}

// forwardingHeaders 是反向代理通常会加上的请求头。
//
// 只要出现其中之一，就说明调用方不是本机直连 —— RemoteAddr 是代理的地址。
var forwardingHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Real-Ip",
	"X-Client-Ip",
	"True-Client-Ip",
	"Forwarded",
}

// viaProxy 报告请求是否经过反向代理。
func viaProxy(r *http.Request) bool {
	for _, h := range forwardingHeaders {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// localDirect 报告请求是否来自「本机直连」。
//
// 管理接口在未设密码时、/v1 在未签发密钥时都靠它做兜底鉴权。
// 只看 RemoteAddr 是不够的：同机反向代理（nginx `proxy_pass 127.0.0.1:10087`）
// 会把**所有**公网请求的 RemoteAddr 变成 127.0.0.1，从而把管理接口
// （含导出完整登录态）暴露给公网。
func (s *Server) localDirect(r *http.Request) bool {
	return !viaProxy(r) && isLoopbackAddr(r.RemoteAddr)
}

// isLoopbackAddr 判断 TCP 远端地址是否为本机环回。
func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// persist 保存状态文件，失败时记日志。
//
// 控制台各操作遍布 Save 调用，逐一处理错误会淹没业务逻辑；但**绝不能静默忽略** ——
// 状态文件里含 STS 凭据与 refresh_token，写失败（磁盘满 / 文件被占用）
// 意味着签到记录、密钥、设置会悄悄丢失。
func (s *Server) persist() {
	if err := s.Store.Save(); err != nil {
		log.Printf("保存状态文件失败: %v", err)
	}
}

// resolveOptions 把设置转换为凭据解析策略。
func (s *Server) resolveOptions() codearts.ResolveOptions {
	st := s.Store.Settings()
	ttl := time.Duration(st.LiveTTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return codearts.ResolveOptions{
		LiveTTL:      ttl,
		Margin:       5 * time.Minute,
		AllowRefresh: st.AutoRefresh,
		WriteBack:    st.WriteBackToIDE,
	}
}

// sanitizedSettings 返回可下发给前端的设置（管理密码不外泄）。
func sanitizedSettings(st store.Settings) map[string]any {
	return map[string]any{
		"model_aliases":       st.ModelAliases,
		"default_model":       st.DefaultModel,
		"force_model":         st.ForceModel,
		"request_timeout_sec": st.RequestTimeout,
		"max_retries":         st.MaxRetries,
		"log_keep":            st.LogKeep,
		"cors_origin":         st.CORSOrigin,
		"auto_refresh":        st.AutoRefresh,
		"allow_credit_models": st.AllowCreditModels,
		"allow_anonymous":     st.AllowAnonymous,
		"auto_claim":          st.AutoClaim,
		"claim_after_hour":    st.ClaimAfterHour,
		"claim_after_minute":  st.ClaimAfterMinute,
		"write_back_to_ide":   st.WriteBackToIDE,
		"live_ttl_sec":        st.LiveTTLSeconds,
		"user_data_dir":       st.UserDataDir,
		"login_port":          st.LoginPort,
		"plugin_name":         st.PluginName,
		"plugin_version":      st.PluginVersion,
		"ide_name":            st.IDEName,
		"ide_version":         st.IDEVersion,
		"language":            st.Language,
		"admin_password_set":  st.AdminPassword != "",
	}
}

// resolveKeyRef 把完整密钥或脱敏密钥解析为完整密钥。
func (s *Server) resolveKeyRef(ref string) string {
	for _, k := range s.Store.Keys() {
		if k.Key == ref || codearts.Mask(k.Key) == ref {
			return k.Key
		}
	}
	return ref
}

func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
		return strings.TrimSpace(h)
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// --- 基础端点 ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accts := s.Store.Accounts()
	ok := 0
	for _, a := range accts {
		if a.Enabled && a.Status == "ok" {
			ok++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"accounts": len(accts),
		"ready":    ok,
		"models":   len(s.Reg.List(nil)),
	})
}

// handleRoot 返回控制台单页；其余未知路径返回 404。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeErr(w, http.StatusNotFound, "未找到该路径", "invalid_request_error")
		return
	}
	serveConsole(w, r)
}

// --- 工具函数 ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg, typ string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}

// intQuery 读取整数查询参数。
func intQuery(r *http.Request, key string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(key))
	return n
}
