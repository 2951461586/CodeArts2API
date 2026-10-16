package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"codearts2api/internal/store"
)

// TestV1AuthAnonymousOnlyFromLocal 回归：未签发密钥时，匿名调用只允许本机直连。
//
// 回归背景：以前只要没签发密钥就无条件放行，而 Dockerfile 的默认 CMD 是
// `-host 0.0.0.0`，一旦端口对外暴露就是白嫖账号额度。
func TestV1AuthAnonymousOnlyFromLocal(t *testing.T) {
	s := newTestServer(t, 0, 1)
	h := s.withAuth(okHandler())

	cases := []struct {
		name    string
		addr    string
		headers map[string]string
		want    int
	}{
		{"本机直连", "127.0.0.1:5555", nil, http.StatusOK},
		{"远端直连", "203.0.113.7:5555", nil, http.StatusUnauthorized},
		{"代理转发", "127.0.0.1:5555", map[string]string{"X-Forwarded-For": "203.0.113.7"}, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, adminRequest(c.addr, c.headers))
			if rec.Code != c.want {
				t.Errorf("状态码 = %d，期望 %d", rec.Code, c.want)
			}
		})
	}
}

// TestV1AuthAllowAnonymousOptIn 显式打开 allow_anonymous 后才允许非本机匿名调用。
func TestV1AuthAllowAnonymousOptIn(t *testing.T) {
	s := newTestServer(t, 0, 1)
	s.Store.UpdateSettings(func(st *store.Settings) { st.AllowAnonymous = true })
	h := s.withAuth(okHandler())

	rec := httptest.NewRecorder()
	h(rec, adminRequest("203.0.113.7:5555", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("开启 allow_anonymous 后状态码 = %d，期望 200", rec.Code)
	}
}

// TestV1AuthRequiresValidKeyOnceIssued 一旦签发了密钥，任何来源都必须携带有效密钥。
func TestV1AuthRequiresValidKeyOnceIssued(t *testing.T) {
	s := newTestServer(t, 0, 1)
	key := s.Store.AddKey("test")
	h := s.withAuth(okHandler())

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"无密钥", nil, http.StatusUnauthorized},
		{"错误密钥", map[string]string{"Authorization": "Bearer sk-ca-nope"}, http.StatusUnauthorized},
		{"正确密钥", map[string]string{"Authorization": "Bearer " + key.Key}, http.StatusOK},
		{"x-api-key 形式", map[string]string{"x-api-key": key.Key}, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h(rec, adminRequest("203.0.113.7:5555", c.headers))
			if rec.Code != c.want {
				t.Errorf("状态码 = %d，期望 %d", rec.Code, c.want)
			}
		})
	}

	// 停用后立即失效
	s.Store.ToggleKey(key.Key)
	rec := httptest.NewRecorder()
	h(rec, adminRequest("203.0.113.7:5555", map[string]string{"Authorization": "Bearer " + key.Key}))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("停用密钥后状态码 = %d，期望 401", rec.Code)
	}
}
