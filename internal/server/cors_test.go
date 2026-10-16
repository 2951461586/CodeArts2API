package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"codearts2api/internal/store"
)

// TestCORSDefaultIsSameOriginOnly 回归：默认不再放行任意 localhost 来源。
//
// 回归背景：以前默认放行 localhost / 127.0.0.1 / [::1] 的任意端口，
// 等于本机随便一个 Web 服务都能读到 /admin/api/* 的响应。
func TestCORSDefaultIsSameOriginOnly(t *testing.T) {
	s := newTestServer(t, 0, 1)

	cases := []struct {
		name   string
		origin string
		host   string
		want   string
	}{
		{"同源", "http://127.0.0.1:10087", "127.0.0.1:10087", "http://127.0.0.1:10087"},
		{"同源（不同写法的主机名）", "http://localhost:10087", "localhost:10087", "http://localhost:10087"},
		{"本机其他端口不再默认放行", "http://localhost:3000", "127.0.0.1:10087", ""},
		{"本机其他端口（127.0.0.1）", "http://127.0.0.1:3000", "127.0.0.1:10087", ""},
		{"外部来源", "https://evil.example.com", "127.0.0.1:10087", ""},
		{"null 来源", "null", "127.0.0.1:10087", ""},
		{"非法来源", "not a url", "127.0.0.1:10087", ""},
		{"无 Origin", "", "127.0.0.1:10087", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if got := s.corsOrigin(r); got != c.want {
				t.Errorf("corsOrigin = %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestCORSExplicitAllowList 配置了 cors_origin 后按白名单放行，且拒绝通配符。
func TestCORSExplicitAllowList(t *testing.T) {
	s := newTestServer(t, 0, 1)
	s.Store.UpdateSettings(func(st *store.Settings) {
		st.CORSOrigin = "http://192.168.1.10:8080, https://app.example.com"
	})

	for _, origin := range []string{"http://192.168.1.10:8080", "https://app.example.com"} {
		r := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
		r.Host = "127.0.0.1:10087"
		r.Header.Set("Origin", origin)
		if got := s.corsOrigin(r); got != origin {
			t.Errorf("白名单来源 %q 被拒绝（got %q）", origin, got)
		}
	}

	// 白名单生效时不再回退到同源逻辑
	r := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	r.Host = "127.0.0.1:10087"
	r.Header.Set("Origin", "http://127.0.0.1:10087")
	if got := s.corsOrigin(r); got != "" {
		t.Errorf("白名单模式下未列出的来源被放行: %q", got)
	}

	// 通配符永不生效
	s.Store.UpdateSettings(func(st *store.Settings) { st.CORSOrigin = "*" })
	r = httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	r.Host = "127.0.0.1:10087"
	r.Header.Set("Origin", "https://evil.example.com")
	if got := s.corsOrigin(r); got != "" {
		t.Errorf("通配符配置放行了外部来源: %q", got)
	}
}

// TestAdminSameOriginWriteGuard 写操作的同源校验。
func TestAdminSameOriginWriteGuard(t *testing.T) {
	s := newTestServer(t, 0, 1)

	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"无 Origin（CLI / curl）", "", "127.0.0.1:10087", true},
		{"同源", "http://127.0.0.1:10087", "127.0.0.1:10087", true},
		{"跨站", "https://evil.example.com", "127.0.0.1:10087", false},
		{"null 来源", "null", "127.0.0.1:10087", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/admin/api/settings", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if got := s.adminSameOrigin(r); got != c.want {
				t.Errorf("adminSameOrigin = %v，期望 %v", got, c.want)
			}
		})
	}
}
