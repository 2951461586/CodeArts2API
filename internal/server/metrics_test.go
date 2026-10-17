package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codearts2api/internal/codearts"
	"codearts2api/internal/store"
)

// TestMetricsExposesStats 校验 /metrics 的输出格式与关键指标。
func TestMetricsExposesStats(t *testing.T) {
	s := newTestServer(t, 0, 1)
	// 三个账号分别处于 ok / error / disabled，用于验证按状态分组。
	// 注意 UpsertAccount 按 AK 去重，所以每个账号必须用不同的 AccessKey。
	for i, spec := range []struct {
		id      string
		enabled bool
		status  string
	}{
		{"a", true, "ok"},
		{"b", true, "error"},
		{"c", false, "ok"},
	} {
		s.Store.UpsertAccount(&codearts.Account{
			ID: spec.id, Name: spec.id, Enabled: spec.enabled, Status: spec.status,
			Session: &codearts.Session{AccessKey: "ak" + string(rune('0'+i)), SecretKey: "sk"},
		})
	}
	// 制造一点统计量。
	s.Store.Record(store.LogEntry{Model: "m", Status: 200}, 42)
	s.Store.Record(store.LogEntry{Model: "m", Status: 500, Err: "boom"}, 0)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:12345" // 本机直连（未设管理密码时允许）
	rec := httptest.NewRecorder()
	s.withAdminAuth(s.handleMetrics)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q，期望 text/plain", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"codearts2api_requests_total 2",
		"codearts2api_requests_failed_total 1",
		"codearts2api_output_chars_total 42",
		"codearts2api_accounts_ready 1",
		`codearts2api_accounts{status="ok"} 1`,
		`codearts2api_accounts{status="error"} 1`,
		`codearts2api_accounts{status="disabled"} 1`,
		"codearts2api_models",
		"codearts2api_uptime_seconds",
		"# TYPE codearts2api_requests_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("指标输出缺少 %q\n---\n%s", want, body)
		}
	}
}

// TestMetricsRequiresAdminAuth 未设密码时，非本机直连必须被拒绝 ——
// 与其它管理接口保持同一套鉴权模型。
func TestMetricsRequiresAdminAuth(t *testing.T) {
	s := newTestServer(t, 0, 1)

	// httptest 默认 RemoteAddr 是 192.0.2.1，不是环回地址。
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	s.withAdminAuth(s.handleMetrics)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("非本机直连状态码 = %d，期望 403（body: %s）", rec.Code, rec.Body.String())
	}

	// 设了管理密码后，携带正确密码即可访问（Prometheus 用 Bearer）。
	s.Store.UpdateSettings(func(st *store.Settings) { st.AdminPassword = "pw" })
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "203.0.113.9:12345"
	req.Header.Set("Authorization", "Bearer pw")
	rec = httptest.NewRecorder()
	s.withAdminAuth(s.handleMetrics)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带正确密码状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}
}
