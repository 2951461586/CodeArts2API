package store

import (
	"path/filepath"
	"sync"
	"testing"

	"codearts2api/internal/codearts"
)

// TestAccountSnapshotsAreIsolated 验证对外只暴露快照：
// 改快照不会影响 store，改 store 也不会影响已发出的快照。
//
// 回归背景：Accounts/Account/NextAccount 曾经直接返回内部指针，
// 控制台的 toggle / clear-errors 与请求路径的 MarkAccountResult
// 会在没有锁保护的情况下读写同一块内存。
func TestAccountSnapshotsAreIsolated(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertAccount(&codearts.Account{
		ID: "a1", Name: "n1", Enabled: true, Status: "ok",
		Session: &codearts.Session{AccessKey: "ak1", SecretKey: "sk1"},
	})

	// 1) 改快照不影响 store
	snap := st.Account("a1")
	snap.Enabled = false
	snap.Failures = 99
	snap.Status = "hacked"
	fresh := st.Account("a1")
	if !fresh.Enabled || fresh.Failures != 0 || fresh.Status != "ok" {
		t.Errorf("修改快照污染了 store: enabled=%v failures=%d status=%q",
			fresh.Enabled, fresh.Failures, fresh.Status)
	}

	// 2) 改 store 不影响已发出的快照
	snap2 := st.Account("a1")
	st.SetAccountEnabled("a1", false)
	st.MarkAccountResult("a1", false, "boom")
	if !snap2.Enabled || snap2.Status != "ok" || snap2.Failures != 0 {
		t.Errorf("store 的变更穿透到了旧快照: enabled=%v status=%q failures=%d",
			snap2.Enabled, snap2.Status, snap2.Failures)
	}
	if fresh2 := st.Account("a1"); fresh2.Enabled || fresh2.Status != "error" || fresh2.Failures != 1 {
		t.Errorf("store 自身的变更没生效: enabled=%v status=%q failures=%d",
			fresh2.Enabled, fresh2.Status, fresh2.Failures)
	}

	// 3) 入库时拷贝：调用方之后改入参不应影响 store
	acct := &codearts.Account{ID: "a2", Name: "n2", Enabled: true, Session: &codearts.Session{AccessKey: "ak2"}}
	st.UpsertAccount(acct)
	acct.Enabled = false
	if got := st.Account("a2"); !got.Enabled {
		t.Error("UpsertAccount 后修改入参影响了 store")
	}
}

// TestConcurrentAccountAccessIsSafe 并发跑「控制台读 + 请求路径写」，
// 配合 -race 即可发现锁缺失（无 -race 时至少保证不死锁、不 panic）。
func TestConcurrentAccountAccessIsSafe(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		st.UpsertAccount(&codearts.Account{
			ID: string(rune('a' + i)), Enabled: true, Status: "ok",
			Session: &codearts.Session{AccessKey: "ak"},
		})
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				for _, a := range st.Accounts() {
					_ = a.DisplayName()
					_ = a.Status
					_ = a.Session.AccessKey
				}
			}
		}()
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if a := st.NextAccount(); a != nil {
					st.MarkAccountResult(a.ID, i%2 == 0, "")
				}
				st.SetAccountEnabled("a", i%2 == 0)
				st.ClearAccountErrors("a")
			}
		}()
	}
	wg.Wait()
}
