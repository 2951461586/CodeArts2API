package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// refreshableSession 构造一个「已过期、续期材料齐全」的会话。
func refreshableSession(t *testing.T, token string) *Session {
	t.Helper()
	pair, err := newDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		AccountLabel:   token,
		AccessKey:      "old-ak-" + token,
		SecretKey:      "old-sk-" + token,
		ExpiresAt:      time.Now().Add(-time.Hour),
		RefreshToken:   token,
		DPoPPrivateJWK: pair.PrivateKeyJWK,
		DPoPPublicJWK:  pair.PublicKeyJWK,
		CodeVerifier:   "verifier-" + token,
	}
}

// startFakeTokenServer 起一个假的令牌端点，返回它的地址。
//
// onRefresh 在每次收到续期请求时调用（参数是 refresh_token 的值），
// 可以在里面阻塞，用来观察锁的行为。返回的凭据是 new-ak-<token>。
func startFakeTokenServer(t *testing.T, onRefresh func(token string)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		token := r.Form.Get("refresh_token")
		if onRefresh != nil {
			onRefresh(token)
		}
		resp := map[string]any{
			"credentials": map[string]any{
				"access_key_id":     "new-ak-" + token,
				"secret_access_key": "new-sk-" + token,
				"security_token":    "new-sts-" + token,
				"expiration":        time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			},
			"refresh_token": "rotated-" + token,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// managerForTest 构造一个把令牌端点指向假服务器的 Manager。
func managerForTest(tokenURL string) *Manager {
	c := NewClient()
	c.TokenURL = tokenURL
	return NewManager(c)
}

// TestManagerRefreshesDifferentAccountsInParallel 回归：一个账号的续期不能挡住别的账号。
//
// 回归背景：Manager 曾用一把全局互斥锁把整个解析（含续期网络调用）串起来，
// 于是一个账号的续期会让所有其他账号的请求排队 —— 模型测活的 4 路并发在
// 凭据解析阶段退化成串行，8 个模型的最坏耗时可被放大到分钟级。
func TestManagerRefreshesDifferentAccountsInParallel(t *testing.T) {
	slowEntered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	tokenURL := startFakeTokenServer(t, func(token string) {
		if token != "slow" {
			return
		}
		once.Do(func() { close(slowEntered) })
		<-release
	})

	m := managerForTest(tokenURL)
	slow := &Account{ID: "slow-acct", Name: "slow", Session: refreshableSession(t, "slow")}
	other := &Account{ID: "other-acct", Name: "other", Session: refreshableSession(t, "other")}

	slowDone := make(chan error, 1)
	go func() {
		_, err := m.Session(context.Background(), slow, ResolveOptions{AllowRefresh: true})
		slowDone <- err
	}()

	// 等慢账号真的进了续期 —— 此刻它持有自己的账号锁。
	select {
	case <-slowEntered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("慢账号没有进入续期")
	}

	// 另一个账号必须能独立完成解析（若锁是全局的，这里会一直等到 release）。
	otherDone := make(chan error, 1)
	go func() {
		_, err := m.Session(context.Background(), other, ResolveOptions{AllowRefresh: true})
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			close(release)
			t.Fatalf("另一个账号解析失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("一个账号续期期间另一个账号被阻塞：锁的粒度仍然是全局的")
	}

	close(release)
	select {
	case err := <-slowDone:
		if err != nil {
			t.Fatalf("慢账号续期失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("释放后慢账号仍未完成")
	}
}

// TestManagerRefreshesSameAccountOnce 回归：同一账号的并发解析只能续期一次。
//
// refresh_token 是「单次使用 + 轮转」的：两个 goroutine 同时续期会互相作废
// （上游 STS5.1806）。所以按账号串行是必须的，并且排在后面的 goroutine 必须
// 直接复用前一个刚拿到的会话，而不是拿已轮转的 token 再续一次。
func TestManagerRefreshesSameAccountOnce(t *testing.T) {
	var refreshes int32
	tokenURL := startFakeTokenServer(t, func(string) { atomic.AddInt32(&refreshes, 1) })

	m := managerForTest(tokenURL)
	// 两个独立的账号快照：模拟两个并发请求各自从 store 取到快照。
	snapshots := []*Account{
		{ID: "same-acct", Name: "same", Session: refreshableSession(t, "same")},
		{ID: "same-acct", Name: "same", Session: refreshableSession(t, "same")},
	}

	type result struct {
		i    int
		sess *Session
	}
	results := make(chan result, len(snapshots))
	var wg sync.WaitGroup
	for i, acct := range snapshots {
		wg.Add(1)
		go func(i int, acct *Account) {
			defer wg.Done()
			s, err := m.Session(context.Background(), acct, ResolveOptions{AllowRefresh: true})
			if err != nil {
				t.Errorf("并发解析失败: %v", err)
				return
			}
			results <- result{i: i, sess: s}
		}(i, acct)
	}
	wg.Wait()
	close(results)

	got := make([]*Session, len(snapshots))
	for r := range results {
		got[r.i] = r.sess
	}

	if n := atomic.LoadInt32(&refreshes); n != 1 {
		t.Fatalf("同一账号被续期 %d 次，期望 1 次（refresh_token 单次使用）", n)
	}
	if got[0] == nil || got[1] == nil {
		t.Fatal("有 goroutine 没拿到会话")
	}
	if got[0].AccessKey != got[1].AccessKey {
		t.Errorf("两个请求拿到的凭据不一致: %q vs %q", got[0].AccessKey, got[1].AccessKey)
	}
	if got[0].AccessKey != "new-ak-same" {
		t.Errorf("没有用上续期后的凭据: %q", got[0].AccessKey)
	}
}

// TestManagerPermanentCredentialNeedsNoLock 确认永久 AK/SK 不进入续期分支
// （它没有续期概念，取锁后立刻返回）。
func TestManagerPermanentCredentialNeedsNoLock(t *testing.T) {
	startFakeTokenServer(t, func(token string) {
		t.Errorf("永久凭据不该发起续期，却收到了 %q 的请求", token)
	})
	acct, err := ManualAKSKAccount("perm", "AKID", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(NewClient())
	sess, err := m.Session(context.Background(), acct, ResolveOptions{AllowRefresh: true})
	if err != nil {
		t.Fatalf("永久凭据解析失败: %v", err)
	}
	if !sess.IsPermanent() {
		t.Fatal("解析结果丢了永久标记")
	}
}
