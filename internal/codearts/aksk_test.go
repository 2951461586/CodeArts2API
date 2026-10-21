package codearts

import (
	"testing"
	"time"
)

// TestPermanentAKSKNeverExpires 是本次修复的核心不变量。
//
// 永久 AK/SK 的 ExpiresAt 是零值。修复前 Valid() 把「零值」一律当成不可用，
// 于是 ManualAccount 填的 110 分钟 TTL 一过，账号就永久失效且无法续期
// （HasRefresh 为 false，Manager 也救不回来）。
func TestPermanentAKSKNeverExpires(t *testing.T) {
	acct, err := ManualAKSKAccount("perm", "AKID", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	s := acct.Session
	if s == nil {
		t.Fatal("session not built")
	}
	if !s.IsPermanent() {
		t.Fatal("aksk account must be marked permanent")
	}
	if s.SecurityToken != "" {
		t.Fatalf("permanent AK/SK must not carry a security token, got %q", s.SecurityToken)
	}
	if !s.ExpiresAt.IsZero() {
		t.Fatalf("permanent AK/SK must have zero expiry, got %v", s.ExpiresAt)
	}

	// 无论提前量多大都必须有效——不能因为「快过期」被判定不可用。
	for _, margin := range []time.Duration{0, time.Minute, 5 * time.Minute, 24 * time.Hour, 100 * 365 * 24 * time.Hour} {
		if !s.Valid(margin) {
			t.Fatalf("permanent credential judged invalid with margin=%s", margin)
		}
	}
}

// TestPermanentAKSKDoesNotClaimRefresh 永久凭据没有续期概念，
// HasRefresh 必须是 false，且 ExpiresIn 不能返回负数或垃圾值。
func TestPermanentAKSKDoesNotClaimRefresh(t *testing.T) {
	acct, err := ManualAKSKAccount("perm", "AKID", "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	s := acct.Session
	if s.HasRefresh() {
		t.Fatal("permanent AK/SK must not advertise refresh capability")
	}
	if got := s.ExpiresIn(); got != 0 {
		t.Fatalf("ExpiresIn()=%s, want 0 for permanent credential", got)
	}
}

// TestManualAKSKRequiresKeys 缺少 AK 或 SK 必须拒绝，避免落一个永远签不了名的账号。
func TestManualAKSKRequiresKeys(t *testing.T) {
	cases := []struct{ name, ak, sk string }{
		{"no-ak", "", "SECRET"},
		{"no-sk", "AKID", ""},
		{"both-empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ManualAKSKAccount("n", tc.ak, tc.sk); err == nil {
				t.Fatal("expected error for missing key material")
			}
		})
	}
}

// TestManualSTSStillExpires 守住原有行为：STS 临时凭据仍然按过期时间判定。
func TestManualSTSStillExpires(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	acct, err := ManualAccount("sts", "AKID", "SECRET", "STS", expired)
	if err != nil {
		t.Fatal(err)
	}
	s := acct.Session
	if s.IsPermanent() {
		t.Fatal("STS credential must not be marked permanent")
	}
	if s.Valid(0) {
		t.Fatal("expired STS credential judged valid")
	}
}

// TestManualSTSDefaultsToTTL STS 未给过期时间时仍按默认 TTL 推定，
// 与永久 AK/SK 的行为区分开。
func TestManualSTSDefaultsToTTL(t *testing.T) {
	acct, err := ManualAccount("sts", "AKID", "SECRET", "STS", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s := acct.Session
	if s.ExpiresAt.IsZero() {
		t.Fatal("STS credential should receive a default TTL")
	}
	if !s.Valid(time.Minute) {
		t.Fatal("freshly created STS credential should be valid")
	}
	if s.IsPermanent() {
		t.Fatal("STS credential must not be marked permanent")
	}
}
