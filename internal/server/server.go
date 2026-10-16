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
	s.Cred.Persist = func(*codearts.Account) error { return st.Save() }
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
	mux.HandleFunc("/admin/api/login/start", s.withAdminAuth(s.handleAdminLoginStart))
	mux.HandleFunc("/admin/api/login/poll", s.withAdminAuth(s.handleAdminLoginPoll))
	mux.HandleFunc("/admin/api/login/cancel", s.withAdminAuth(s.handleAdminLoginCancel))

	mux.HandleFunc("/", s.handleRoot)

	return s.withCORS(mux)
}

// withCORS 处理跨域。
//
// 默认只放行本机来源（localhost / 127.0.0.1 / [::1]，任意端口）；
// 需局域网访问时在设置里显式列出逗号分隔的来源。不提供通配符。
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := s.corsOrigin(r.Header.Get("Origin"))
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
func (s *Server) corsOrigin(raw string) string {
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
	if !isLoopbackHost(u.Hostname()) {
		return ""
	}
	if port := u.Port(); port != "" {
		return u.Scheme + "://" + net.JoinHostPort(u.Hostname(), port)
	}
	return u.Scheme + "://" + u.Hostname()
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

// withAuth 校验 Bearer 密钥。
//
// 未签发任何密钥时，允许无密钥访问（首次使用的便利）；
// 一旦签发了密钥，则强制校验。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Store.HasKeys() {
			next(w, r)
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
//   - 未设置管理密码：仅允许来自本机环回的请求。
//
// 另外对写操作做同源校验，阻断跨站请求伪造。
func (s *Server) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pw := s.Store.Settings().AdminPassword; pw != "" {
			if !secureEqual(adminPasswordFrom(r), pw) {
				writeErr(w, http.StatusUnauthorized, "管理密码缺失或错误", "invalid_request_error")
				return
			}
		} else if !isLoopbackAddr(r.RemoteAddr) {
			writeErr(w, http.StatusForbidden, "未设置管理密码，管理接口仅允许本机访问", "invalid_request_error")
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

// isLoopbackAddr 判断 TCP 远端地址是否为本机环回。
func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		"auto_claim":          st.AutoClaim,
		"claim_after_hour":    st.ClaimAfterHour,
		"claim_after_minute":  st.ClaimAfterMinute,
		"write_back_to_ide":   st.WriteBackToIDE,
		"live_ttl_sec":        st.LiveTTLSeconds,
		"user_data_dir":       st.UserDataDir,
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
