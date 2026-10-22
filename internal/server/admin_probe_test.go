package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"codearts2api/internal/codearts"
	"codearts2api/internal/ratelimit"
	"codearts2api/internal/registry"
	"codearts2api/internal/store"
)

// --- 测试脚手架 ---

// probeRoundTripFunc 把上游 HTTP 调用替换成内存应答，测试因此不碰网络。
type probeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f probeRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// probeFakeResponse 构造一个由 fake transport 返回的响应。
func probeFakeResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// newAdminTestServer 构造带凭据管理器与限流器的 Server。
//
// 账号使用**永久 AK/SK**（kindAKSK）：Manager 解析凭据时直接命中缓存分支，
// 不会发起续期网络请求，测试因此只需伪造推理端点。
func newAdminTestServer(t *testing.T, ids ...string) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateSettings(func(s *store.Settings) {
		s.RateLimitEnabled = false
		s.MaxRetries = 1
	})
	up := codearts.NewClient()
	s := &Server{Store: st, Up: up, Reg: registry.New(), cache: newModelCache()}
	s.Cred = codearts.NewManager(up)
	s.Limits = ratelimit.New(rateLimitConfig(st.Settings()), nil)
	for _, id := range ids {
		st.UpsertAccount(&codearts.Account{
			ID: id, Name: "acct-" + id, Enabled: true, Status: "ok",
			Session: &codearts.Session{
				AccessKey: "ak" + id, SecretKey: "sk" + id,
				CredentialKind: codearts.CredentialKindAKSK,
			},
		})
	}
	return s
}

// adminProbeRequest 走完整的路由 + 管理鉴权链（RemoteAddr 设为环回地址，模拟本机直连）。
func adminProbeRequest(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.RemoteAddr = "127.0.0.1:12345"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// --- 纯函数 ---

// TestDedupeModels 校验去空白、去空值、去重。
//
// 重复 ID 会变成两次真实上游请求（真实扣积分），调用方没有任何理由需要重复探测。
func TestDedupeModels(t *testing.T) {
	got := dedupeModels([]string{" glm-5.3 ", "", "glm-5.3", "GLM-5.2", "  ", "GLM-5.2"})
	want := []string{"glm-5.3", "GLM-5.2"}
	if len(got) != len(want) {
		t.Fatalf("dedupeModels = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %q，期望 %q", i, got[i], want[i])
		}
	}
	if empty := dedupeModels(nil); len(empty) != 0 {
		t.Errorf("nil 输入应返回空切片，实际 %v", empty)
	}
}

// --- 测活接口的闸门 ---

// TestModelProbeRejectsTooManyModels 一次请求的模型数有上限：每个模型都是真实扣费。
func TestModelProbeRejectsTooManyModels(t *testing.T) {
	s := newAdminTestServer(t, "a")
	models := make([]string, probeMaxModels+1)
	for i := range models {
		models[i] = fmt.Sprintf("m%d", i)
	}
	body, _ := json.Marshal(map[string]any{"models": models})

	rec := adminProbeRequest(t, s, http.MethodPost, "/admin/api/models/probe", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body: %s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprint(probeMaxModels)) {
		t.Errorf("错误信息应说明上限 %d，实际：%s", probeMaxModels, rec.Body.String())
	}
}

// TestModelProbeNoAccounts 没有账号时直接报错，不能返回一个空成功的假象。
func TestModelProbeNoAccounts(t *testing.T) {
	s := newAdminTestServer(t) // 无账号

	rec := adminProbeRequest(t, s, http.MethodPost, "/admin/api/models/probe", `{"models":["glm-5.3"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body: %s）", rec.Code, rec.Body.String())
	}
}

// TestModelProbeRejectsConcurrentRun 同一时刻只允许一个测活任务，否则成本翻倍
// 且会打满单账号 3 个并发会话的上限、测出假「不可用」。
func TestModelProbeRejectsConcurrentRun(t *testing.T) {
	s := newAdminTestServer(t, "a")
	s.probeMu.Lock()
	defer s.probeMu.Unlock()

	rec := adminProbeRequest(t, s, http.MethodPost, "/admin/api/models/probe", `{"models":["glm-5.3"]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（body: %s）", rec.Code, rec.Body.String())
	}
}

// --- 测活结果 ---

// TestModelProbeReportsPerModelResult 逐个模型给出可用性，且成功/失败互不污染。
func TestModelProbeReportsPerModelResult(t *testing.T) {
	s := newAdminTestServer(t, "a")
	s.Up.HTTP = &http.Client{Transport: probeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Model == "bad-model" {
			// 上游用 HTTP 200 + error_code 表达业务失败。
			return probeFakeResponse(r, http.StatusOK, `{"error_code":"4005","error_msg":"unsupported model"}`), nil
		}
		return probeFakeResponse(r, http.StatusOK, `{"choices":[{"index":0,"message":{"content":"hi"}}]}`), nil
	})}

	rec := adminProbeRequest(t, s, http.MethodPost, "/admin/api/models/probe",
		`{"models":["good-model","bad-model"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}

	var resp struct {
		Total       int              `json:"total"`
		Available   int              `json:"available"`
		Unavailable int              `json:"unavailable"`
		Results     []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v（body: %s）", err, rec.Body.String())
	}
	if resp.Total != 2 || resp.Available != 1 || resp.Unavailable != 1 {
		t.Errorf("计数不对：total=%d available=%d unavailable=%d，期望 2/1/1",
			resp.Total, resp.Available, resp.Unavailable)
	}

	byModel := map[string]map[string]any{}
	for _, row := range resp.Results {
		byModel[fmt.Sprint(row["model"])] = row
	}
	if ok, _ := byModel["good-model"]["ok"].(bool); !ok {
		t.Errorf("good-model 应可用，实际 %v", byModel["good-model"])
	}
	if ok, _ := byModel["bad-model"]["ok"].(bool); ok {
		t.Errorf("bad-model 应不可用，实际 %v", byModel["bad-model"])
	}
	if msg := fmt.Sprint(byModel["bad-model"]["error"]); !strings.Contains(msg, "unsupported model") {
		t.Errorf("bad-model 的错误信息丢了上游原因：%q", msg)
	}
}

// TestModelProbeFallsBackToNextAccount 一个账号不可用时，换下一个账号再试；
// 任一账号可用即判定该模型可用，并回填实际使用的账号名。
func TestModelProbeFallsBackToNextAccount(t *testing.T) {
	s := newAdminTestServer(t, "a", "b")
	var calls int32
	s.Up.HTTP = &http.Client{Transport: probeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		// 账号 a（AK=aka）失败，账号 b（AK=akb）成功。
		if strings.Contains(r.Header.Get("Authorization"), "Access=aka") {
			return probeFakeResponse(r, http.StatusInternalServerError, "boom"), nil
		}
		return probeFakeResponse(r, http.StatusOK, `{"choices":[{"message":{"content":"hi"}}]}`), nil
	})}

	rec := adminProbeRequest(t, s, http.MethodPost, "/admin/api/models/probe",
		`{"models":["glm-5.3"],"id":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}
	var resp struct {
		Available int              `json:"available"`
		Results   []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Available != 1 {
		t.Fatalf("应有一个账号可用，实际 available=%d（body: %s）", resp.Available, rec.Body.String())
	}
	if got := fmt.Sprint(resp.Results[0]["account"]); got != "acct-b" {
		t.Errorf("应回填实际成功的账号 acct-b，实际 %q", got)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Errorf("应至少尝试两个账号，实际只调用上游 %d 次", calls)
	}
}

// TestModelProbeEmptyListUsesRegistry 未指定 models 时探测当前清单里的全部上游模型，
// 别名不参与（它指向的仍是上游真实模型）。
func TestModelProbeEmptyListUsesRegistry(t *testing.T) {
	s := newAdminTestServer(t, "a")
	// 测活按 4 路并发调用上游，假 transport 会被多个 goroutine 同时进入，
	// 记录探测过的模型必须加锁（否则 -race 会报 write/write 竞争）。
	var mu sync.Mutex
	var probed []string
	s.Up.HTTP = &http.Client{Transport: probeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		probed = append(probed, req.Model)
		mu.Unlock()
		return probeFakeResponse(r, http.StatusOK, `{"choices":[{"message":{"content":"hi"}}]}`), nil
	})}

	rec := adminProbeRequest(t, s, http.MethodPost, "/admin/api/models/probe", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body: %s）", rec.Code, rec.Body.String())
	}

	var resp struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != len(registry.Fallback) {
		t.Errorf("应探测兜底清单里的 %d 个上游模型，实际 %d", len(registry.Fallback), resp.Total)
	}
	// 别名不应出现在探测列表里。
	mu.Lock()
	defer mu.Unlock()
	for _, id := range probed {
		if id == "gpt-4o" || id == "claude-sonnet-4" {
			t.Errorf("别名 %q 不应被单独测活", id)
		}
	}
}
