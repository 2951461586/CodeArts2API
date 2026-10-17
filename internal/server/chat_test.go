package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"codearts2api/internal/codearts"
	"codearts2api/internal/registry"
)

// --- 重试退避 ---

// TestTransientBackoffSequence 钉住退避序列：base、2*base，然后封顶。
//
// 回归背景：退避原本是一条内联表达式（`n * base`，封顶 max），
// 写错成 0 或常量不会有任何测试失败 —— 线上只表现为「重试等于没退避」，
// 上游并发上限（TM.00001041）依然会被连续打满。
func TestTransientBackoffSequence(t *testing.T) {
	want := []time.Duration{
		1 * time.Second, // 第 1 次
		2 * time.Second, // 第 2 次
		3 * time.Second, // 第 3 次：2*base=2s < max=3s
		3 * time.Second, // 第 4 次：3*base=3s，等于上限
		3 * time.Second, // 第 5 次：4*base=4s，被封顶
	}
	for i, w := range want {
		if got := transientBackoff(i + 1); got != w {
			t.Errorf("transientBackoff(%d) = %v，期望 %v", i+1, got, w)
		}
	}
	if got := transientBackoff(0); got != 0 {
		t.Errorf("transientBackoff(0) = %v，期望 0", got)
	}
	if got := transientBackoff(-1); got != 0 {
		t.Errorf("transientBackoff(-1) = %v，期望 0", got)
	}
}

// TestWithAccountRetryBackoffWired 确认退避真的接在重试路径上（不只是有个纯函数）。
//
// 用 waitRetry 钩子记录每次实际等待时长，避免测试真的睡 3 秒。
func TestWithAccountRetryBackoffWired(t *testing.T) {
	var waits []time.Duration
	restore := waitRetry
	waitRetry = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	t.Cleanup(func() { waitRetry = restore })

	s := newTestServer(t, 1, 3)
	_, err := s.withAccountRetry(t.Context(), 5*time.Second, func(context.Context, *codearts.Account) error {
		return transientErr()
	})
	if err == nil {
		t.Fatal("全部尝试都是临时性故障时应返回错误")
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("退避了 %d 次（%v），期望 %d 次（%v）", len(waits), waits, len(want), want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Errorf("第 %d 次退避 = %v，期望 %v", i+1, waits[i], want[i])
		}
	}
}

// --- 临时性故障不计账号失败 ---

// TestWithAccountRetryTransientThenSuccess 临时性故障后重试成功，账号必须保持健康。
//
// 与 TestWithAccountRetryTransientKeepsAccountHealthy（全部失败）互补：
// 这里验证「重试成功」这条最常见的路径也不会留下失败计数。
func TestWithAccountRetryTransientThenSuccess(t *testing.T) {
	restore := waitRetry
	waitRetry = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { waitRetry = restore })

	s := newTestServer(t, 1, 3)

	var attempts int
	used, err := s.withAccountRetry(t.Context(), 5*time.Second, func(context.Context, *codearts.Account) error {
		attempts++
		if attempts == 1 {
			return transientErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("重试成功后仍返回错误: %v", err)
	}
	if attempts != 2 {
		t.Errorf("尝试了 %d 次，期望 2 次", attempts)
	}
	if used == nil || used.ID != "a" {
		t.Fatalf("返回的账号 = %v，期望 a", used)
	}
	acct := s.Store.Account("a")
	if acct.Failures != 0 {
		t.Errorf("临时性故障后成功，账号失败次数 = %d，期望 0", acct.Failures)
	}
	if acct.Status != "ok" {
		t.Errorf("账号状态 = %q，期望 ok", acct.Status)
	}
	if acct.LastErr != "" {
		t.Errorf("账号被写入了错误信息: %q", acct.LastErr)
	}
}

// --- 流式 → 非流式聚合兜底 ---

// roundTripFunc 让测试用假的 Transport 顶替真实网络。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// sseFixture 是上游流式响应的最小分片序列（两段正文 + 结束标记）。
const sseFixture = `data: {"id":"chatcmpl-fix","object":"chat.completion.chunk","created":1700000000,"model":"glm-5.3","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}

data: {"id":"chatcmpl-fix","object":"chat.completion.chunk","created":1700000000,"model":"glm-5.3","choices":[{"index":0,"delta":{"content":"世界"},"finish_reason":"stop"}]}

data: [DONE]

`

// TestChatOnceFallsBackToStreamAggregation 上游对非流式返回「空 body 的 400」时，
// 网关必须自动改走「流式 + 本地聚合」，对客户端保持非流式语义。
//
// 回归背景：实测 glm-5.3 的非流式接口只回一个空 body 的 400，若不兜底，
// 客户端拿到的就是 400；同时该模型应被标记为 StreamOnly，避免下次再打空枪。
func TestChatOnceFallsBackToStreamAggregation(t *testing.T) {
	s := newTestServer(t, 1, 1)
	s.Cred = codearts.NewManager(s.Up)
	s.Reg.Replace([]registry.Model{{ID: "glm-5.3", Channel: codearts.ChannelBenefit}})

	// 凭据必须「有效」，否则 Manager 会走续期分支（本测试不覆盖续期）。
	s.Store.SetAccountSession("a", &codearts.Session{
		AccessKey:     "ak",
		SecretKey:     "sk",
		SecurityToken: "st",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	if sess := s.Store.Account("a").Session; sess == nil || !sess.Valid(time.Minute) {
		t.Fatal("测试前置失败：账号凭据不可用，chatOnce 会走续期分支")
	}

	var (
		mu     sync.Mutex
		bodies []string
	)
	s.Up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		n := len(bodies)
		mu.Unlock()

		if n == 1 {
			// 第一次：非流式请求 → 空 body 的 400（IsEmptyRejection 的判定条件）
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    r,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseFixture)),
			Request:    r,
		}, nil
	})}

	acct := s.Store.Account("a")
	model := registry.Model{ID: "glm-5.3", Channel: codearts.ChannelBenefit}
	raw, err := s.chatOnce(t.Context(), acct, model, []byte(`{"model":"glm-5.3","stream":false,"messages":[]}`))
	if err != nil {
		t.Fatalf("聚合兜底失败: %v", err)
	}

	// 1) 聚合结果必须是完整的非流式响应
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v\n%s", err, raw)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q，期望 chat.completion", got.Object)
	}
	if got.Model != "glm-5.3" {
		t.Errorf("model = %q，期望 glm-5.3", got.Model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices 数量 = %d，期望 1", len(got.Choices))
	}
	if c := got.Choices[0]; c.Message.Role != "assistant" || c.Message.Content != "你好世界" {
		t.Errorf("聚合正文 = %q/%q，期望 assistant/你好世界", c.Message.Role, c.Message.Content)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q，期望 stop", got.Choices[0].FinishReason)
	}

	// 2) 请求序列：先非流式，失败后改流式
	mu.Lock()
	first, second := bodies[0], bodies[1]
	count := len(bodies)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("上游被请求 %d 次，期望 2 次（非流式 + 流式兜底）", count)
	}
	if !strings.Contains(first, `"stream":false`) {
		t.Errorf("第一次请求不是非流式: %s", first)
	}
	if !strings.Contains(second, `"stream":true`) {
		t.Errorf("兜底请求没有改成流式: %s", second)
	}

	// 3) 该模型应被记为 StreamOnly，下次不再打空枪。
	//
	// 生产路径每个请求都会重新 Resolve 出模型（见 handleChatCompletions），
	// 所以这里也要重新取一次，否则拿到的还是调用方手里的旧副本。
	model = s.Reg.Resolve("glm-5.3", nil)
	if !model.StreamOnly {
		t.Fatal("模型没有被标记为 StreamOnly，下次仍会先发一次必然失败的非流式请求")
	}
	if _, err := s.chatOnce(t.Context(), acct, model, []byte(`{"model":"glm-5.3","stream":false,"messages":[]}`)); err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Errorf("第二次调用共请求 %d 次，期望 3 次（直接走流式）", len(bodies))
	}
	if !strings.Contains(bodies[2], `"stream":true`) {
		t.Errorf("StreamOnly 模型仍然先发了非流式请求: %s", bodies[2])
	}
}
