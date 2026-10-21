package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codearts2api/internal/store"
)

// postSettings 直接调用设置接口（绕过 HTTP 鉴权，只测字段语义）。
func postSettings(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/settings", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.handleAdminSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	return out
}

// TestAdminPasswordEmptyDoesNotClear 回归：空的 admin_password 不能把密码抹掉。
//
// 回归背景：以前 `if body.AdminPassword != nil` 直接赋值，于是任何带
// `"admin_password": ""` 的请求都会清除管理密码，让管理接口退回
// 「本机直连」兜底 —— 配合反向代理就是一个静默提权链。
func TestAdminPasswordEmptyDoesNotClear(t *testing.T) {
	s := newTestServer(t, 0, 1)
	s.Store.UpdateSettings(func(st *store.Settings) { st.AdminPassword = "keepme" })

	postSettings(t, s, `{"admin_password":""}`)
	if got := s.Store.Settings().AdminPassword; got != "keepme" {
		t.Errorf("空字符串清除了管理密码: %q", got)
	}

	postSettings(t, s, `{"admin_password":"   "}`)
	if got := s.Store.Settings().AdminPassword; got != "keepme" {
		t.Errorf("空白字符串清除了管理密码: %q", got)
	}

	// 不带该字段（null）也不应改动
	postSettings(t, s, `{"default_model":"GLM-5.2"}`)
	if got := s.Store.Settings().AdminPassword; got != "keepme" {
		t.Errorf("未携带该字段却改动了管理密码: %q", got)
	}
}

// TestAdminPasswordSetAndClear 设置与显式清除。
func TestAdminPasswordSetAndClear(t *testing.T) {
	s := newTestServer(t, 0, 1)

	postSettings(t, s, `{"admin_password":"  s3cret  "}`)
	if got := s.Store.Settings().AdminPassword; got != "s3cret" {
		t.Errorf("设置管理密码失败（应去掉两端空白）: %q", got)
	}

	// 不带 clear 标志的 false 不应清除
	postSettings(t, s, `{"clear_admin_password":false}`)
	if got := s.Store.Settings().AdminPassword; got != "s3cret" {
		t.Errorf("clear_admin_password=false 却清除了密码: %q", got)
	}

	postSettings(t, s, `{"clear_admin_password":true}`)
	if got := s.Store.Settings().AdminPassword; got != "" {
		t.Errorf("显式清除失败: %q", got)
	}
}

// TestSanitizedSettingsHidesAdminPassword 下发给前端的设置里不能出现密码本体。
func TestSanitizedSettingsHidesAdminPassword(t *testing.T) {
	st := store.DefaultSettings()
	st.AdminPassword = "topsecret"
	out := sanitizedSettings(st)

	if _, ok := out["admin_password"]; ok {
		t.Error("sanitizedSettings 泄露了 admin_password 字段")
	}
	if out["admin_password_set"] != true {
		t.Errorf("admin_password_set = %v，期望 true", out["admin_password_set"])
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "topsecret") {
		t.Error("序列化后的设置里包含管理密码明文")
	}
}
