// Package ratelimit 提供按密钥的自我保护型限流：令牌桶（QPS）+ 在飞请求上限（并发）。
//
// 定位见 goal 的「非目标」：网关是单机个人使用，限流只做**自我保护** ——
// 防止失控客户端（循环重试、Agent 死循环、多标签页）烧光账号积分，
// 不承诺密钥之间的公平配额，也不做排队。
//
// 本包是叶子包：不依赖任何内部包，可脱离 HTTP 单测（时钟可注入）。
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Config 是限流参数，按**单个密钥**生效。
type Config struct {
	// Enabled 关闭时不拦截任何请求。
	Enabled bool
	// QPS 是每秒补充的令牌数，即长期允许的请求速率。
	QPS int
	// Burst 是桶容量，即允许的瞬时突发请求数。
	Burst int
	// Concurrency 是同一密钥允许的在飞请求上限；<= 0 表示不限。
	Concurrency int
}

// Limiter 按 key 维护令牌桶与在飞计数。
//
// 桶只增不删：key 是已通过鉴权的 API 密钥（匿名时是固定单桶），
// 数量被已签发密钥数封顶，因此不需要 GC。
type Limiter struct {
	mu      sync.Mutex
	cfg     Config
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens   float64
	last     time.Time
	inflight int
}

// New 构造限流器。now 为 nil 时使用 time.Now；测试可注入假时钟，避免 sleep。
func New(cfg Config, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{cfg: cfg, buckets: make(map[string]*bucket), now: now}
}

// SetConfig 热更新参数。已经在飞的请求不受影响，只影响后续判断。
func (l *Limiter) SetConfig(cfg Config) {
	l.mu.Lock()
	l.cfg = cfg
	l.mu.Unlock()
}

// Acquire 尝试为 key 取得一个执行许可。
//
// 成功时返回的 release **必须**调用（通常 defer）：它负责把并发计数还回去，
// 漏调会让该密钥在 Concurrency 次请求之后永久 429。release 可重复调用。
//
// 失败时 retryAfter 是建议客户端等待的时长，恒大于 0。
func (l *Limiter) Acquire(key string) (release func(), retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cfg := l.cfg
	if !cfg.Enabled || cfg.QPS <= 0 {
		return func() {}, 0, true
	}
	burst := cfg.Burst
	if burst < 1 {
		burst = 1
	}

	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(burst), last: now}
		l.buckets[key] = b
	}

	// 按流逝时间补充令牌，上限为桶容量。
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(float64(burst), b.tokens+elapsed*float64(cfg.QPS))
		b.last = now
	}

	// 先看并发、再扣令牌：并发被拒时不必回滚令牌。
	if cfg.Concurrency > 0 && b.inflight >= cfg.Concurrency {
		return nil, time.Second, false
	}
	if b.tokens < 1 {
		return nil, retryAfterFor(b.tokens, cfg.QPS), false
	}

	b.tokens--
	b.inflight++
	return l.releaser(b), 0, true
}

// retryAfterFor 计算补满 1 个令牌还要多久，向上取整到秒、至少 1 秒。
func retryAfterFor(tokens float64, qps int) time.Duration {
	wait := (1 - tokens) / float64(qps)
	if wait <= 0 {
		return time.Second
	}
	return time.Duration(math.Ceil(wait)) * time.Second
}

// releaser 返回并发安全、可重复调用的释放函数。
func (l *Limiter) releaser(b *bucket) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if b.inflight > 0 {
				b.inflight--
			}
			l.mu.Unlock()
		})
	}
}
