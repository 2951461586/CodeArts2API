package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"codearts2api/internal/store"
)

// okHandler 是一个永远返回 200 的处理器，用来观察鉴权是否放行。
func okHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
}

// adminRequest 构造一个带指定来源地址 / 请求头的管理接口请求。
func adminRequest(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/admin/api/state", nil)
	r.RemoteAddr = remoteAddr
	r.Host = "127.0.0.1:10087"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestLocalDirectRejectsProxyHeaders 回归：同机反向代理会让所有请求的
// RemoteAddr 都变成 127.0.0.1，因此只看 RemoteAddr 会把管理接口暴露给公网。
func TestLocalDirectRejectsProxyHeaders(t *testing.T) {
	s := newTestServer(t, 0, 1)

	cases := []struct {
		name    string
		addr    string
		headers map[string]string
		want    bool
	}{
		{"本机直连", "127.0.0.1:5555", nil, true},
		{"IPv6 环回", "[::1]:5555", nil, true},
		{"远端直连", "203.0.113.7:5555", nil, false},
		{"代理转发 X-Forwarded-For", "127.0.0.1:5555", map[string]string{"X-Forwarded-For": "203.0.113.7"}, false},
		{"代理转发 X-Real-Ip", "127.0.0.1:5555", map[string]string{"X-Real-Ip": "203.0.113.7"}, false},
		{"代理转发 Forwarded", "127.0.0.1:5555", map[string]string{"Forwarded": "for=203.0.113.7"}, false},
		{"代理转发 X-Forwarded-Proto", "127.0.0.1:5555", map[string]string{"X-Forwarded-Proto": "https"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.localDirect(adminRequest(c.addr, c.headers)); got != c.want {
				t.Errorf("localDirect = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAdminAuthBlocksProxiedRequests 未设密码时，经代理转发的请求必须被拒。
func TestAdminAuthBlocksProxiedRequests(t *testing.T) {
	s := newTestServer(t, 0, 1)
	h := s.withAdminAuth(okHandler())

	rec := httptest.NewRecorder()
	h(rec, adminRequest("127.0.0.1:5555", map[string]string{"X-Forwarded-For": "203.0.113.7"}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("代理转发请求的状态码 = %d，期望 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	h(rec, adminRequest("203.0.113.7:5555", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("远端直连的状态码 = %d，期望 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	h(rec, adminRequest("127.0.0.1:5555", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("本机直连的状态码 = %d，期望 200", rec.Code)
	}
}

// TestAdminAuthPasswordTakesPrecedence 设了密码后，任何来源都必须带密码。
func TestAdminAuthPasswordTakesPrecedence(t *testing.T) {
	s := newTestServer(t, 0, 1)
	s.Store.UpdateSettings(func(st *store.Settings) { st.AdminPassword = "s3cret" })
	h := s.withAdminAuth(okHandler())

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"无密码", nil, http.StatusUnauthorized},
		{"错误密码", map[string]string{"X-Admin-Password": "nope"}, http.StatusUnauthorized},
		{"正确密码", map[string]string{"X-Admin-Password": "s3cret"}, http.StatusOK},
		{"Bearer 形式", map[string]string{"Authorization": "Bearer s3cret"}, http.StatusOK},
		{"本机直连也必须带密码", nil, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, adminRequest("127.0.0.1:5555", c.headers))
			if rec.Code != c.want {
				t.Errorf("状态码 = %d，期望 %d", rec.Code, c.want)
			}
		})
	}
}

// TestSecureEqual 恒定时间比较的基本正确性。
func TestSecureEqual(t *testing.T) {
	if !secureEqual("abc", "abc") {
		t.Error("相同字符串应相等")
	}
	if secureEqual("abc", "abd") || secureEqual("abc", "ab") || secureEqual("", "x") {
		t.Error("不同字符串不应相等")
	}
	if !secureEqual("", "") {
		t.Error("两个空串应相等")
	}
}
