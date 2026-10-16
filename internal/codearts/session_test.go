package codearts

import (
	"testing"
	"time"
)

// TestSessionValidRejectsUnknownExpiry 回归：过期时间未知时不能当成「永不过期」。
//
// 回归背景：Valid() 曾经在 ExpiresAt 为零时直接 return true（注释还写着
// 「按 STS 常规有效期保守估算」，实现却是永远有效）。后果是凭据实际已失效时
// 网关会一直拿着死凭据打上游，永远不触发续期。
func TestSessionValidRejectsUnknownExpiry(t *testing.T) {
	var nilSess *Session
	if nilSess.Valid(time.Minute) {
		t.Error("nil Session 不应有效")
	}

	cases := []struct {
		name string
		sess *Session
		want bool
	}{
		{"缺少 AK/SK", &Session{ExpiresAt: time.Now().Add(time.Hour)}, false},
		{"过期时间未知", &Session{AccessKey: "ak", SecretKey: "sk"}, false},
		{"尚未过期", &Session{AccessKey: "ak", SecretKey: "sk", ExpiresAt: time.Now().Add(time.Hour)}, true},
		{"已过期", &Session{AccessKey: "ak", SecretKey: "sk", ExpiresAt: time.Now().Add(-time.Minute)}, false},
		{
			"落在 margin 之内（需要提前续期）",
			&Session{AccessKey: "ak", SecretKey: "sk", ExpiresAt: time.Now().Add(2 * time.Minute)},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sess.Valid(5 * time.Minute); got != c.want {
				t.Errorf("Valid = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestParseSessionFillsUnknownExpiry 会话里缺 expires_at 时按保守 TTL 估算。
func TestParseSessionFillsUnknownExpiry(t *testing.T) {
	before := time.Now()
	sess, err := parseSession([]byte(`{"accessKey":"AK","secretKey":"SK"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sess.ExpiresAt.IsZero() {
		t.Fatal("缺 expires_at 时应补一个保守的过期时间")
	}
	want := before.Add(DefaultCredentialTTL)
	if diff := sess.ExpiresAt.Sub(want); diff > time.Minute || diff < -time.Minute {
		t.Errorf("补的过期时间偏离预期: %v", diff)
	}
	if !sess.Valid(5 * time.Minute) {
		t.Error("刚解析出来的会话应当可用")
	}
}

// TestParseSessionKeepsExplicitExpiry 显式的 expires_at 必须原样保留。
func TestParseSessionKeepsExplicitExpiry(t *testing.T) {
	raw := []byte(`{"accessKey":"AK","secretKey":"SK","expires_at":"2030-01-02T03:04:05.000Z"}`)
	sess, err := parseSession(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if !sess.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v，期望 %v", sess.ExpiresAt, want)
	}
}

// TestSessionExpiresIn 剩余时长计算。
func TestSessionExpiresIn(t *testing.T) {
	var nilSess *Session
	if got := nilSess.ExpiresIn(); got != 0 {
		t.Errorf("nil Session 的 ExpiresIn = %v，期望 0", got)
	}
	if got := (&Session{}).ExpiresIn(); got != 0 {
		t.Errorf("过期时间未知时 ExpiresIn = %v，期望 0", got)
	}
	sess := &Session{ExpiresAt: time.Now().Add(30 * time.Minute)}
	if got := sess.ExpiresIn(); got <= 0 || got > 31*time.Minute {
		t.Errorf("ExpiresIn = %v，期望约 30 分钟", got)
	}
}
