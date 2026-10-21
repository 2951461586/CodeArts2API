package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"codearts2api/internal/store"
)

// rateLimitedServer 构造一个限流参数被压到最小的服务：桶容量 1、qps 1。
// 这样第二次请求必定被拒，不需要 sleep。
func rateLimitedServer(t *testing.T, enabled bool) *Server {
	t.Helper()
	s := newTestServer(t, 0, 1)
	s.Store.UpdateSettings(func(st *store.Settings) {
		st.RateLimitEnabled = enabled
		st.RateLimitQPS = 1
		st.RateLimitBurst = 1
		st.RateLimitConcurrency = 8
	})
	s.ApplyRateLimit()
	return s
}

// v1Request 构造一个带 Bearer 密钥的推理请求。
func v1Request(path, key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// TestRateLimitReturns429WithRetryAfter 验证超限后返回 429、带 Retry-After，
// 并且被拦下的请求根本没有进入业务处理器（不消耗账号额度）。
func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	s := rateLimitedServer(t, true)
	key := s.Store.AddKey("test").Key

	var reached int
	h := s.withAuth(s.withRateLimit(errOpenAI, func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))

	// 第一次：桶里唯一一个令牌。
	rec := httptest.NewRecorder()
	h(rec, v1Request("/v1/chat/completions", key))
	if rec.Code != http.StatusOK {
		t.Fatalf("第一次请求状态码 = %d，期望 200", rec.Code)
	}

	// 第二次：桶空，必须被拒。
	rec = httptest.NewRecorder()
	h(rec, v1Request("/v1/chat/completions", key))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限状态码 = %d，期望 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q，期望 \"1\"", got)
	}
	if reached != 1 {
		t.Errorf("业务处理器被调用 %d 次，期望 1 次（被限流的请求不应进入业务逻辑）", reached)
	}

	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q，期望 rate_limit_error", body.Error.Type)
	}
	if body.Error.Message == "" {
		t.Error("error.message 不应为空")
	}
}

// TestRateLimitAnthropicErrorShape 验证 /v1/messages 上返回的是 Anthropic 错误结构，
// 而不是 OpenAI 的 {"error":{...}} —— 两套 SDK 的解析器不通用。
func TestRateLimitAnthropicErrorShape(t *testing.T) {
	s := rateLimitedServer(t, true)
	key := s.Store.AddKey("test").Key
	h := s.withAuth(s.withRateLimit(errAnthropic, okHandler()))

	// 先用掉唯一的令牌。
	h(httptest.NewRecorder(), v1Request("/v1/messages", key))

	rec := httptest.NewRecorder()
	h(rec, v1Request("/v1/messages", key))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限状态码 = %d，期望 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("Anthropic 风格也必须带 Retry-After")
	}

	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if body.Type != "error" {
		t.Errorf("顶层 type = %q，期望 error", body.Type)
	}
	if body.Error.Type != "rate_limit_error" {
		t.Errorf("error.type = %q，期望 rate_limit_error", body.Error.Type)
	}
}

// TestRateLimitDisabledDoesNotThrottle 验证关掉限流后不再拦截。
func TestRateLimitDisabledDoesNotThrottle(t *testing.T) {
	s := rateLimitedServer(t, false)
	key := s.Store.AddKey("test").Key
	h := s.withAuth(s.withRateLimit(errOpenAI, okHandler()))

	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		h(rec, v1Request("/v1/chat/completions", key))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求状态码 = %d，关闭限流后应为 200", i+1, rec.Code)
		}
	}
}

// TestRateLimitKeysAreIndependent 验证一个密钥打满不会影响另一个。
func TestRateLimitKeysAreIndependent(t *testing.T) {
	s := rateLimitedServer(t, true)
	keyA := s.Store.AddKey("a").Key
	keyB := s.Store.AddKey("b").Key
	h := s.withAuth(s.withRateLimit(errOpenAI, okHandler()))

	h(httptest.NewRecorder(), v1Request("/v1/chat/completions", keyA))
	if rec := httptest.NewRecorder(); func() int {
		h(rec, v1Request("/v1/chat/completions", keyA))
		return rec.Code
	}() != http.StatusTooManyRequests {
		t.Fatal("key a 用尽令牌后应被拒绝")
	}

	rec := httptest.NewRecorder()
	h(rec, v1Request("/v1/chat/completions", keyB))
	if rec.Code != http.StatusOK {
		t.Fatalf("key b 状态码 = %d，期望 200（不同密钥互不影响）", rec.Code)
	}
}

// TestRateLimitSkipsUnauthenticated 验证鉴权先于限流：无效密钥得到 401 而不是 429。
func TestRateLimitSkipsUnauthenticated(t *testing.T) {
	s := rateLimitedServer(t, true)
	s.Store.AddKey("test") // 已签发密钥 → 必须带有效密钥
	h := s.withAuth(s.withRateLimit(errOpenAI, okHandler()))

	rec := httptest.NewRecorder()
	h(rec, v1Request("/v1/chat/completions", "sk-ca-invalid"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（鉴权应在限流之前）", rec.Code)
	}
}
