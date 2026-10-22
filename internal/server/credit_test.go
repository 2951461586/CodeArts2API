package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codearts2api/internal/codearts"
)

// creditBody 是一份正常的积分额度响应。
const creditBody = `{
  "package":{"status":"normal","is_credit_package":true,"package_name_cn":"基础套餐"},
  "metrics":[{"name":"usageTotalPackageCredit","package_credit_amount":1000,
              "package_credit_used":250,"package_credit_remain":750,"value":25}],
  "end_date":"2026-11-01"
}`

// fakeCreditUpstream 用固定响应替换上游统计插件接口，并统计调用次数。
func fakeCreditUpstream(t *testing.T, status int, body string, calls *int32) *http.Client {
	t.Helper()
	return &http.Client{Transport: probeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		return probeFakeResponse(r, status, body), nil
	})}
}

// TestAdminCreditsAllAccounts 返回每个账号的额度，且成功/失败互不影响。
func TestAdminCreditsAllAccounts(t *testing.T) {
	s := newAdminTestServer(t, "a", "b")
	s.Up.HTTP = fakeCreditUpstream(t, http.StatusOK, creditBody, nil)

	rec := adminProbeRequest(t, s, http.MethodGet, "/admin/api/credits", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}

	var resp struct {
		Accounts []struct {
			ID     string               `json:"id"`
			Name   string               `json:"name"`
			OK     bool                 `json:"ok"`
			Error  string               `json:"error"`
			Credit *codearts.CreditInfo `json:"credit"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v（body: %s）", err, rec.Body.String())
	}
	if len(resp.Accounts) != 2 {
		t.Fatalf("应返回 2 个账号，实际 %d", len(resp.Accounts))
	}
	for _, a := range resp.Accounts {
		if !a.OK {
			t.Errorf("账号 %s 查询失败：%s", a.ID, a.Error)
			continue
		}
		if a.Credit == nil || !a.Credit.Available {
			t.Errorf("账号 %s 没有额度数据", a.ID)
			continue
		}
		if a.Credit.Remaining != 750 || a.Credit.Total != 1000 {
			t.Errorf("账号 %s 额度 = %v/%v，期望 750/1000",
				a.ID, a.Credit.Remaining, a.Credit.Total)
		}
	}
}

// TestAdminCreditsSingleAccount ?id= 只查一个账号。
func TestAdminCreditsSingleAccount(t *testing.T) {
	s := newAdminTestServer(t, "a", "b")
	s.Up.HTTP = fakeCreditUpstream(t, http.StatusOK, creditBody, nil)

	rec := adminProbeRequest(t, s, http.MethodGet, "/admin/api/credits?id=b", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}
	var resp struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Accounts) != 1 || resp.Accounts[0].ID != "b" {
		t.Errorf("应只返回账号 b，实际 %+v", resp.Accounts)
	}
}

// TestAdminCreditsUpstreamError 上游失败时该账号标记为 ok=false，不影响其它账号。
func TestAdminCreditsUpstreamError(t *testing.T) {
	s := newAdminTestServer(t, "a")
	s.Up.HTTP = fakeCreditUpstream(t, http.StatusInternalServerError, "boom", nil)

	rec := adminProbeRequest(t, s, http.MethodGet, "/admin/api/credits", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}
	var resp struct {
		Accounts []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Accounts) != 1 || resp.Accounts[0].OK {
		t.Fatalf("上游 500 时该账号应失败，实际 %+v", resp.Accounts)
	}
	if resp.Accounts[0].Error == "" {
		t.Error("应返回失败原因")
	}
}

// TestAdminCreditsCache 默认走 60 秒缓存，?fresh=1 强制回源。
func TestAdminCreditsCache(t *testing.T) {
	s := newAdminTestServer(t, "a")
	var calls int32
	s.Up.HTTP = fakeCreditUpstream(t, http.StatusOK, creditBody, &calls)

	for i := 0; i < 2; i++ {
		rec := adminProbeRequest(t, s, http.MethodGet, "/admin/api/credits", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次状态码 = %d", i+1, rec.Code)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("缓存未生效：上游被调用 %d 次，期望 1", got)
	}

	rec := adminProbeRequest(t, s, http.MethodGet, "/admin/api/credits?fresh=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh 状态码 = %d", rec.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("?fresh=1 应回源：上游被调用 %d 次，期望 2", got)
	}
}

// TestAdminCreditsNoAccounts 没有账号时明确报错。
func TestAdminCreditsNoAccounts(t *testing.T) {
	s := newAdminTestServer(t)
	rec := adminProbeRequest(t, s, http.MethodGet, "/admin/api/credits", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body: %s）", rec.Code, rec.Body.String())
	}
}

// TestAdminCreditsMethodNotAllowed 只接受 GET。
func TestAdminCreditsMethodNotAllowed(t *testing.T) {
	s := newAdminTestServer(t, "a")
	rec := adminProbeRequest(t, s, http.MethodPost, "/admin/api/credits", `{}`)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405（body: %s）", rec.Code, rec.Body.String())
	}
}

// TestAdminCreditsPermanentCredential 永久 AK/SK 账号也能查额度（不需要续期）。
func TestAdminCreditsPermanentCredential(t *testing.T) {
	s := newAdminTestServer(t, "a")
	s.Up.HTTP = fakeCreditUpstream(t, http.StatusOK, creditBody, nil)
	rec := adminProbeRequest(t, s, http.MethodGet, "/admin/api/credits?id=a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（body: %s）", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !containsAll(body, `"available":true`, `"remaining":750`) {
		t.Errorf("响应缺少额度字段：%s", body)
	}
}

// containsAll 报告 s 是否包含所有子串。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
