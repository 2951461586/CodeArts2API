package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock 是可推进的假时钟，避免测试里出现 sleep。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newLimiter(cfg Config) (*Limiter, *fakeClock) {
	clock := newFakeClock()
	return New(cfg, clock.Now), clock
}

// TestDisabledAllowsEverything 回归：关闭限流时必须无条件放行，且不建桶。
func TestDisabledAllowsEverything(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: false, QPS: 1, Burst: 1, Concurrency: 1})
	for i := 0; i < 100; i++ {
		release, retryAfter, ok := l.Acquire("k")
		if !ok {
			t.Fatalf("第 %d 次被拒绝，关闭限流时不应拒绝", i)
		}
		if retryAfter != 0 {
			t.Fatalf("放行时不应给 retryAfter，得到 %v", retryAfter)
		}
		release()
	}
	if len(l.buckets) != 0 {
		t.Fatalf("关闭时不应创建桶，实际 %d 个", len(l.buckets))
	}
}

// TestBurstThenReject 验证桶容量就是允许的瞬时突发数。
func TestBurstThenReject(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: true, QPS: 1, Burst: 3, Concurrency: 0})
	for i := 0; i < 3; i++ {
		if _, _, ok := l.Acquire("k"); !ok {
			t.Fatalf("突发内第 %d 次不应被拒绝", i+1)
		}
	}
	_, retryAfter, ok := l.Acquire("k")
	if ok {
		t.Fatal("超过 burst 后应当拒绝")
	}
	// tokens=0，qps=1 → 补满 1 个令牌要 1 秒
	if retryAfter != time.Second {
		t.Fatalf("retryAfter 应为 1s，得到 %v", retryAfter)
	}
}

// TestRefillOverTime 验证令牌按流逝时间补充。
func TestRefillOverTime(t *testing.T) {
	l, clock := newLimiter(Config{Enabled: true, QPS: 2, Burst: 1, Concurrency: 0})

	if _, _, ok := l.Acquire("k"); !ok {
		t.Fatal("首次请求应放行")
	}
	if _, _, ok := l.Acquire("k"); ok {
		t.Fatal("桶空时应拒绝")
	}

	clock.Advance(250 * time.Millisecond) // 0.25s * 2qps = 0.5 个令牌，不够
	if _, retryAfter, ok := l.Acquire("k"); ok {
		t.Fatal("半秒补充的令牌不足 1 个，应继续拒绝")
	} else if retryAfter != time.Second {
		// (1-0.5)/2 = 0.25s → 向上取整到 1 秒
		t.Fatalf("retryAfter 应向上取整到 1s，得到 %v", retryAfter)
	}

	clock.Advance(250 * time.Millisecond) // 累计 0.5s * 2qps = 1 个令牌
	if _, _, ok := l.Acquire("k"); !ok {
		t.Fatal("令牌补满后应放行")
	}
}

// TestConcurrencyCap 验证在飞请求上限，以及 release 后名额归还。
func TestConcurrencyCap(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: true, QPS: 100, Burst: 100, Concurrency: 2})

	rel1, _, ok := l.Acquire("k")
	if !ok {
		t.Fatal("第 1 个请求应放行")
	}
	rel2, _, ok := l.Acquire("k")
	if !ok {
		t.Fatal("第 2 个请求应放行")
	}

	_, retryAfter, ok := l.Acquire("k")
	if ok {
		t.Fatal("超过并发上限应当拒绝")
	}
	if retryAfter != time.Second {
		t.Fatalf("并发拒绝的 retryAfter 应为 1s，得到 %v", retryAfter)
	}

	rel1()
	rel1() // 重复释放必须安全，且不能多退名额
	if _, _, ok := l.Acquire("k"); !ok {
		t.Fatal("释放一个名额后应能再取一个")
	}
	if _, _, ok := l.Acquire("k"); ok {
		t.Fatal("名额只释放了一个，不应放行两个")
	}
	rel2()
}

// TestConcurrencyRejectionDoesNotBurnToken 验证并发被拒时不消耗令牌。
func TestConcurrencyRejectionDoesNotBurnToken(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: true, QPS: 1, Burst: 1, Concurrency: 1})

	rel, _, ok := l.Acquire("k")
	if !ok {
		t.Fatal("首次请求应放行")
	}
	// 桶里还剩 0 个令牌；这次被并发挡住，不应再扣令牌（也没得扣）。
	if _, _, ok := l.Acquire("k"); ok {
		t.Fatal("并发满时应拒绝")
	}
	rel()
	// 并发名额回来了，但令牌仍然是 0 → 仍应拒绝，说明前面没有把令牌扣成负数。
	if _, _, ok := l.Acquire("k"); ok {
		t.Fatal("令牌已耗尽，释放并发名额也不应放行")
	}
}

// TestKeysAreIsolated 验证不同密钥互不影响。
func TestKeysAreIsolated(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: true, QPS: 1, Burst: 1, Concurrency: 0})

	if _, _, ok := l.Acquire("a"); !ok {
		t.Fatal("key a 首次应放行")
	}
	if _, _, ok := l.Acquire("a"); ok {
		t.Fatal("key a 应被拒绝")
	}
	if _, _, ok := l.Acquire("b"); !ok {
		t.Fatal("key b 不应受 key a 影响")
	}
}

// TestSetConfigHotReload 验证参数热更新立即生效。
func TestSetConfigHotReload(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: true, QPS: 1, Burst: 1, Concurrency: 0})

	if _, _, ok := l.Acquire("k"); !ok {
		t.Fatal("首次应放行")
	}
	if _, _, ok := l.Acquire("k"); ok {
		t.Fatal("桶空时应拒绝")
	}

	l.SetConfig(Config{Enabled: false})
	if _, _, ok := l.Acquire("k"); !ok {
		t.Fatal("关闭后应立即放行")
	}
}

// TestZeroQPSDoesNotPanic 回归：qps 为 0 时不能除零崩溃（配置兜底失效时的最后一道防线）。
func TestZeroQPSDoesNotPanic(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: true, QPS: 0, Burst: 1, Concurrency: 1})
	if _, _, ok := l.Acquire("k"); !ok {
		t.Fatal("qps<=0 应当视为关闭而不是拒绝")
	}
}

// TestConcurrentAcquireIsSafe 供 -race 使用：并发取用/释放在同一与不同密钥上。
func TestConcurrentAcquireIsSafe(t *testing.T) {
	l, _ := newLimiter(Config{Enabled: true, QPS: 1000, Burst: 1000, Concurrency: 0})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				key := "k"
				if i%2 == 0 {
					key = "other"
				}
				release, _, ok := l.Acquire(key)
				if ok {
					release()
				}
			}
		}(i)
	}
	wg.Wait()
}
