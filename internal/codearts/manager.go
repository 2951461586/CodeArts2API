package codearts

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ResolveOptions 控制凭据解析策略。
type ResolveOptions struct {
	// LiveTTL 是「从 state.vscdb 重读」的缓存时长。IDE 运行时它会持续续期，
	// 因此取一个较短的值就能自动跟随，同时避免每次请求都解析 SQLite。
	LiveTTL time.Duration
	// Margin 是提前量：剩余有效期小于它即视为需要续期。
	Margin time.Duration
	// AllowRefresh 允许在凭据过期且 IDE 未续期时，自行用 refresh_token 续期。
	AllowRefresh bool
	// WriteBack 允许把续期结果回写 state.vscdb（让 IDE 也拿到新凭据）。
	WriteBack bool
}

// DefaultResolveOptions 返回推荐策略。
func DefaultResolveOptions() ResolveOptions {
	return ResolveOptions{
		LiveTTL:      10 * time.Second,
		Margin:       5 * time.Minute,
		AllowRefresh: true,
		WriteBack:    true,
	}
}

// Manager 负责为账号解析出可用的上游凭据。
//
// 优先级：
//  1. state.vscdb 里的实时会话（IDE 运行时它自己会续期，永远最新）
//  2. 网关本地缓存的会话（IDE 关闭后仍可继续用）
//  3. 用 refresh_token 自行续期（DPoP + PKCE verifier）
type Manager struct {
	Client *Client
	// Persist 在凭据发生变化时回调，用于把新会话写进状态文件。
	Persist func(acct *Account) error
	// Logf 是可选的日志回调。
	Logf func(format string, args ...any)

	mu       sync.Mutex
	liveAt   map[string]time.Time
	liveSess map[string]*Session
}

// NewManager 构造凭据管理器。
func NewManager(c *Client) *Manager {
	return &Manager{
		Client:   c,
		liveAt:   map[string]time.Time{},
		liveSess: map[string]*Session{},
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Session 解析出该账号当前可用的凭据。
func (m *Manager) Session(ctx context.Context, acct *Account, opt ResolveOptions) (*Session, error) {
	if opt.LiveTTL <= 0 {
		opt.LiveTTL = 10 * time.Second
	}
	if opt.Margin <= 0 {
		opt.Margin = 5 * time.Minute
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1) 实时会话（带短缓存）
	live := m.cachedLive(acct, opt.LiveTTL)
	if live == nil && acct.UserData != "" {
		if s, err := ReadSession(acct.UserData); err == nil {
			m.liveAt[acct.ID] = time.Now()
			m.liveSess[acct.ID] = s
			live = s
		} else {
			m.logf("账号 %s 读取 IDE 会话失败: %v", acct.DisplayName(), err)
		}
	}
	if live != nil && live.Valid(opt.Margin) {
		m.adopt(acct, live)
		return live, nil
	}

	// 2) 本地缓存的会话
	if acct.Session != nil && acct.Session.Valid(opt.Margin) {
		return acct.Session, nil
	}

	// 3) 自行续期。优先用实时会话里的 refresh_token（更新），否则用缓存的。
	fresh := live
	if fresh == nil || !fresh.HasRefresh() {
		fresh = acct.Session
	}
	if !opt.AllowRefresh || fresh == nil || !fresh.HasRefresh() {
		if live != nil {
			return nil, fmt.Errorf("账号 %s 的凭据已过期（%s），请在 CodeArts Agent 里重新登录",
				acct.DisplayName(), live.ExpiresAt.Local().Format("2006-01-02 15:04:05"))
		}
		return nil, fmt.Errorf("账号 %s 没有可用凭据，请先在 CodeArts Agent 里登录，或在控制台手工录入",
			acct.DisplayName())
	}

	m.logf("账号 %s 凭据已过期，尝试用 refresh_token 续期", acct.DisplayName())
	res, err := m.Client.Refresh(ctx, fresh)
	if err != nil {
		return nil, fmt.Errorf("账号 %s 续期失败: %w", acct.DisplayName(), err)
	}
	updated := *fresh
	updated.AccessKey = res.AccessKey
	updated.SecretKey = res.SecretKey
	updated.SecurityToken = res.SecurityToken
	updated.ExpiresAt = res.ExpiresAt
	updated.RefreshToken = res.RefreshToken
	updated.Source = "refresh"
	m.logf("账号 %s 续期成功，新凭据有效期至 %s", acct.DisplayName(), res.ExpiresAt.Local().Format("2006-01-02 15:04:05"))

	m.adopt(acct, &updated)
	m.liveSess[acct.ID] = &updated
	m.liveAt[acct.ID] = time.Now()

	if opt.WriteBack && acct.UserData != "" {
		if err := writeBackSession(acct.UserData, &updated); err != nil {
			// 回写失败不影响网关自身使用，仅提示
			m.logf("账号 %s 回写 state.vscdb 失败（网关内仍可正常使用）: %v", acct.DisplayName(), err)
		} else {
			m.logf("账号 %s 新凭据已回写 state.vscdb，IDE 下次启动可直接复用", acct.DisplayName())
		}
	}
	return &updated, nil
}

// cachedLive 返回仍在 TTL 内的实时会话。
func (m *Manager) cachedLive(acct *Account, ttl time.Duration) *Session {
	if acct.UserData == "" {
		return nil
	}
	at, ok := m.liveAt[acct.ID]
	if !ok || time.Since(at) > ttl {
		return nil
	}
	return m.liveSess[acct.ID]
}

// adopt 把新会话记到账号快照上；凭据真的变了就通知持久化。
//
// 凭据轮换（IDE 自行续期、网关自行续期）大约每 2 小时一次，changed 为假时
// 不会触发落盘，因此这里不会造成写盘风暴。
func (m *Manager) adopt(acct *Account, s *Session) {
	changed := acct.Session == nil ||
		acct.Session.AccessKey != s.AccessKey ||
		acct.Session.SecurityToken != s.SecurityToken ||
		acct.Session.RefreshToken != s.RefreshToken
	acct.Session = s
	if changed && m.Persist != nil {
		if err := m.Persist(acct); err != nil {
			m.logf("账号 %s 状态保存失败: %v", acct.DisplayName(), err)
		}
	}
}

// Invalidate 清掉某账号的实时会话缓存（例如用户点了「探活」或刚改过凭据）。
func (m *Manager) Invalidate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.liveAt, id)
	delete(m.liveSess, id)
}

// ImportFromDesktop 扫描本机 CodeArts Agent 的 User Data 目录并导入会话。
func ImportFromDesktop(explicit string) ([]*ImportResult, error) {
	var out []*ImportResult
	var lastErr error
	for _, dir := range DefaultUserDataDirs(explicit) {
		sess, err := ReadSession(dir)
		if err != nil {
			lastErr = err
			continue
		}
		name := sess.AccountLabel
		if name == "" {
			name = "CodeArts " + shortID(sess.DomainID)
		}
		out = append(out, &ImportResult{
			Dir: dir,
			Account: &Account{
				ID:       RandomHex(8),
				Name:     name,
				Source:   "desktop",
				UserData: dir,
				Enabled:  true,
				Status:   "ok",
				Session:  sess,
			},
			AK:      sess.AccessKey,
			Expires: sess.ExpiresAt,
		})
	}
	if len(out) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("未找到 CodeArts Agent 的登录会话")
		}
		return nil, lastErr
	}
	return out, nil
}

// ManualAccount 用手工录入的 AK/SK/STS 构造账号（供 Docker / 跨平台场景使用）。
func ManualAccount(name, ak, sk, sts string, expires time.Time) (*Account, error) {
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("access key / secret key 不能为空")
	}
	if name == "" {
		name = "手工录入 " + shortID(ak)
	}
	if expires.IsZero() {
		expires = time.Now().Add(DefaultCredentialTTL)
	}
	return &Account{
		ID:      RandomHex(8),
		Name:    name,
		Source:  "manual",
		Enabled: true,
		Status:  "ok",
		Session: &Session{
			AccountLabel:  name,
			AccessKey:     ak,
			SecretKey:     sk,
			SecurityToken: sts,
			ExpiresAt:     expires,
			Source:        "manual",
		},
	}, nil
}
