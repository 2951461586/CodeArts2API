package codearts

import (
	"context"
	"testing"
	"time"
)

// TestManagerReturnsPermanentCredentialWithoutRefresh 验证 Manager 对永久 AK/SK
// 走的是「本地缓存直接可用」分支，既不读 state.vscdb，也不尝试续期。
//
// 用 UserData 为空 + Session 为永久凭据的账号：如果实现走进了续期分支，
// 会因为缺 refresh 材料而返回 CredentialsError。
func TestManagerReturnsPermanentCredentialWithoutRefresh(t *testing.T) {
	acct, err := ManualAKSKAccount("perm", "AKID", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil) // Client 为 nil：一旦尝试续期就会 panic，正好守住这条边界

	sess, err := m.Session(context.Background(), acct, ResolveOptions{
		LiveTTL: 10 * time.Second, Margin: 5 * time.Minute,
		AllowRefresh: true, WriteBack: true,
	})
	if err != nil {
		t.Fatalf("permanent credential should resolve without refresh: %v", err)
	}
	if sess == nil || sess.AccessKey != "AKID" || sess.SecretKey != "SECRET" {
		t.Fatalf("unexpected session: %+v", sess)
	}
	if !sess.IsPermanent() {
		t.Fatal("resolved session lost its permanent marker")
	}
}

// TestManagerStillRefreshesExpiredSTS 确认本次改动没有把 STS 的续期路径关掉：
// 过期且无续期材料的 STS 账号必须报错，而不是被当成可用。
func TestManagerStillRefreshesExpiredSTS(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	acct, err := ManualAccount("sts", "AKID", "SECRET", "STS", expired)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil)

	_, err = m.Session(context.Background(), acct, ResolveOptions{
		LiveTTL: 10 * time.Second, Margin: 5 * time.Minute,
		AllowRefresh: true, WriteBack: false,
	})
	if err == nil {
		t.Fatal("expired STS credential without refresh material must fail")
	}
}
