package ratelimit_test

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/authn/ratelimit"
)

// clock is the injected time source. Every lockout and refill assertion below
// names an exact instant; sleeping would be slower, flakier, and would test
// nothing the arithmetic does not already cover.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Unix(1_700_000_000, 0)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// baseConfig uses numbers that divide evenly, so every expectation is a whole
// number of milliseconds: 8 tokens/second means one token every 125ms.
func baseConfig() ratelimit.Config {
	return ratelimit.Config{
		PerSecond:    8,
		Burst:        4,
		MaxFailures:  5,
		LockoutBase:  30 * time.Second,
		MaxLockout:   time.Hour,
		IdleEviction: time.Hour,
	}
}

func newLimiter(cfg ratelimit.Config, c *clock) *ratelimit.Limiter {
	return ratelimit.New(cfg, c.now)
}

// drain consumes the whole bucket, then proves the next request is refused.
func drain(t *testing.T, l *ratelimit.Limiter, key string, burst int) time.Duration {
	t.Helper()

	for i := 0; i < burst; i++ {
		if ok, _ := l.Allow(key); !ok {
			t.Fatalf("Allow %d of %d was denied while the bucket should still hold tokens", i+1, burst)
		}
	}
	ok, retry := l.Allow(key)
	if ok {
		t.Fatalf("Allow succeeded after %d tokens were consumed from a %d-deep bucket", burst, burst)
	}
	if retry <= 0 {
		t.Fatalf("a denied request reported retryAfter %v; a client that waits that long never retries", retry)
	}
	return retry
}

func TestBucketRefillsProportionallyAndCapsAtBurst(t *testing.T) {
	c := newClock()
	l := newLimiter(baseConfig(), c)
	const key = "203.0.113.7"

	drain(t, l, key, 4)

	// Half a token is not a token, and the wait offered must cover the time to
	// the whole one. A small margin above the true wait is deliberate slack
	// against float rounding, so this is a band rather than an equality.
	c.advance(62500 * time.Microsecond)
	if ok, retry := l.Allow(key); ok {
		t.Error("Allow succeeded on half a refilled token")
	} else if want := 62500 * time.Microsecond; retry < want || retry > want+time.Millisecond {
		t.Errorf("retryAfter = %v, want within [%v, %v]", retry, want, want+time.Millisecond)
	}

	c.advance(62500 * time.Microsecond)
	if ok, _ := l.Allow(key); !ok {
		t.Fatal("Allow denied after a whole token had refilled")
	}
	if ok, _ := l.Allow(key); ok {
		t.Error("Allow handed out a second token that had not refilled")
	}

	// Idling must not accumulate credit beyond the burst depth, or one quiet
	// client could spend a hundred requests in a burst.
	c.advance(time.Hour)
	for i := 0; i < 4; i++ {
		if ok, _ := l.Allow(key); !ok {
			t.Fatalf("Allow %d was denied after an idle hour; the bucket should be full", i+1)
		}
	}
	if ok, _ := l.Allow(key); ok {
		t.Error("the bucket held more than Burst tokens after idling")
	}
}

// TestRetryAfterIsEnoughToBeAllowedAgain is the property that matters: a
// retryAfter that is one nanosecond short is a permanent denial, because the
// client waits, is refused, and is handed an even smaller wait each time.
func TestRetryAfterIsEnoughToBeAllowedAgain(t *testing.T) {
	// Exact divisors of a second and rates that are not, because truncating
	// (1/rate) to whole nanoseconds only bites for the latter.
	for _, rate := range []float64{1, 2, 4, 8, 0.5, 2.5, 3, 6, 7, 9} {
		t.Run(strconv.FormatFloat(rate, 'g', -1, 64), func(t *testing.T) {
			cfg := baseConfig()
			cfg.PerSecond = rate
			c := newClock()
			l := newLimiter(cfg, c)
			const key = "client"

			drain(t, l, key, cfg.Burst)

			ok, retry := l.Allow(key)
			if ok {
				t.Fatal("Allow succeeded with an empty bucket")
			}

			c.advance(retry)
			ok, again := l.Allow(key)
			if !ok {
				t.Fatalf("waiting the reported retryAfter (%v) still denies, and the next wait offered is %v: "+
					"told to retry at an instant that never arrives, the client can never make progress",
					retry, again)
			}
		})
	}
}

func TestLockoutEscalatesWithEachFailureBeyondTheTolerance(t *testing.T) {
	// MaxFailures 5, base 30s, cap 1h. Five failures are tolerated; the sixth
	// locks for 30s and each further failure doubles the wait until the cap.
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 0},
		{5, 0}, // the tolerance itself is not a lockout
		{6, 30 * time.Second},
		{7, time.Minute},
		{8, 2 * time.Minute},
		{9, 4 * time.Minute},
		{10, 8 * time.Minute},
		{11, 16 * time.Minute},
		{12, 32 * time.Minute},
		{13, time.Hour}, // 64m would exceed the cap
		{20, time.Hour},
		{40, time.Hour},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%d_failures", tc.failures), func(t *testing.T) {
			c := newClock()
			l := newLimiter(baseConfig(), c)

			for i := 0; i < tc.failures; i++ {
				l.Fail("client")
			}

			if got := l.LockedFor("client"); got != tc.want {
				t.Errorf("after %d failures LockedFor = %v, want %v", tc.failures, got, tc.want)
			}

			ok, retry := l.Allow("client")
			switch tc.want {
			case 0:
				if !ok {
					t.Error("Allow denied a key that has not been locked out")
				}
			default:
				if ok {
					t.Errorf("Allow admitted a key locked out for another %v", tc.want)
				}
				if retry != tc.want {
					t.Errorf("Allow reported retryAfter %v, want %v", retry, tc.want)
				}
			}
		})
	}
}

func TestExpiredLockoutsDoNotResetTheEscalation(t *testing.T) {
	c := newClock()
	l := newLimiter(baseConfig(), c)
	const key = "client"

	for i := 0; i < 6; i++ {
		l.Fail(key)
	}
	if got := l.LockedFor(key); got != 30*time.Second {
		t.Fatalf("LockedFor after 6 failures = %v, want 30s", got)
	}

	// Exactly at the expiry instant the key is usable again, and the seventh
	// failure must escalate rather than start over at the base.
	c.advance(30 * time.Second)
	if got := l.LockedFor(key); got != 0 {
		t.Fatalf("LockedFor at the expiry instant = %v, want 0", got)
	}
	l.Fail(key)
	if got := l.LockedFor(key); got != time.Minute {
		t.Errorf("LockedFor after waiting out the lockout and failing again = %v, want 1m: "+
			"an attacker can reset the escalation by waiting", got)
	}
}

func TestEscalationStaysBoundedForAVeryPersistentAttacker(t *testing.T) {
	// A one-nanosecond base with a huge cap drives the shift past 32, which is
	// where an unguarded 1<<shift would wrap into a negative duration and turn
	// a lockout into an immediate retry.
	cfg := baseConfig()
	cfg.MaxFailures = 0
	cfg.LockoutBase = time.Nanosecond
	cfg.MaxLockout = 1000 * time.Hour

	c := newClock()
	l := newLimiter(cfg, c)
	const key = "client"

	for i := 1; i <= 32; i++ {
		l.Fail(key)
	}
	if got := l.LockedFor(key); got != time.Duration(1)<<31 {
		t.Errorf("LockedFor after 32 failures = %v, want 2.147s (1ns << 31)", got)
	}

	for i := 0; i < 20; i++ {
		l.Fail(key)
	}
	if got := l.LockedFor(key); got != cfg.MaxLockout {
		t.Errorf("LockedFor after 52 failures = %v, want the cap %v", got, cfg.MaxLockout)
	}
}

func TestSucceedClearsFailuresAndLockout(t *testing.T) {
	c := newClock()
	l := newLimiter(baseConfig(), c)
	const key = "client"

	for i := 0; i < 6; i++ {
		l.Fail(key)
	}
	if got := l.LockedFor(key); got == 0 {
		t.Fatal("six failures did not lock the key")
	}

	l.Succeed(key)
	if got := l.LockedFor(key); got != 0 {
		t.Fatalf("LockedFor after Succeed = %v, want 0", got)
	}
	if ok, _ := l.Allow(key); !ok {
		t.Error("Allow denied a key whose lockout Succeed had cleared")
	}

	// The failure count must be gone too, not merely unlocked: five more
	// failures are exactly the tolerance and must not lock the key again.
	for i := 0; i < 5; i++ {
		l.Fail(key)
	}
	if got := l.LockedFor(key); got != 0 {
		t.Errorf("LockedFor = %v after Succeed plus five failures, want 0: Succeed did not "+
			"reset the count", got)
	}
	l.Fail(key)
	if got := l.LockedFor(key); got == 0 {
		t.Error("the sixth failure after Succeed did not lock the key")
	}
}

func TestLockedForReportsRemainingTimeAndZeroWhenUnlocked(t *testing.T) {
	c := newClock()
	l := newLimiter(baseConfig(), c)
	const key = "client"

	if got := l.LockedFor("never-seen"); got != 0 {
		t.Errorf("LockedFor of an unknown key = %v, want 0", got)
	}
	for i := 0; i < 6; i++ {
		l.Fail(key)
	}
	if got := l.LockedFor(key); got != 30*time.Second {
		t.Errorf("LockedFor = %v, want 30s", got)
	}

	c.advance(10 * time.Second)
	if got := l.LockedFor(key); got != 20*time.Second {
		t.Errorf("LockedFor after 10s of a 30s lockout = %v, want 20s", got)
	}

	c.advance(20 * time.Second)
	if got := l.LockedFor(key); got != 0 {
		t.Errorf("LockedFor after the lockout expired = %v, want 0", got)
	}
	if ok, _ := l.Allow(key); !ok {
		t.Error("Allow denied a key whose lockout had expired")
	}
}

func TestKeysAreIndependent(t *testing.T) {
	c := newClock()
	l := newLimiter(baseConfig(), c)

	for i := 0; i < 6; i++ {
		l.Fail("attacker")
	}
	if ok, _ := l.Allow("attacker"); ok {
		t.Error("Allow admitted the locked-out key")
	}
	if ok, _ := l.Allow("bystander"); !ok {
		t.Error("a lockout on one key denied a different key")
	}
}

// TestSweepNeverEvictsAKeyThatIsStillLockedOut is the most important test here:
// if Sweep dropped a locked entry, an attacker could simply stop touching the
// key, wait for the sweep, and find the lockout gone.
func TestSweepNeverEvictsAKeyThatIsStillLockedOut(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFailures = 1
	cfg.LockoutBase = time.Hour
	cfg.MaxLockout = 24 * time.Hour
	cfg.IdleEviction = time.Minute

	c := newClock()
	l := newLimiter(cfg, c)
	const key = "attacker"

	l.Fail(key) // tolerated
	l.Fail(key) // one hour of lockout

	// Idle for far longer than the eviction window, but well inside the
	// lockout. Waiting out IdleEviction must not be a way to buy a retry.
	c.advance(30 * time.Minute)
	l.Sweep()

	if got := l.LockedFor(key); got != 30*time.Minute {
		t.Fatalf("LockedFor after Sweep = %v, want 30m: the sweep evicted a live lockout", got)
	}
	if ok, retry := l.Allow(key); ok {
		t.Fatal("Allow admitted a key whose live lockout the sweep dropped")
	} else if retry != 30*time.Minute {
		t.Errorf("Allow reported retryAfter %v, want 30m", retry)
	}
}

func TestSweepDropsIdleUnlockedKeysSoStateDoesNotGrowForever(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFailures = 1
	cfg.LockoutBase = time.Minute
	cfg.MaxLockout = time.Hour
	cfg.IdleEviction = time.Minute

	c := newClock()
	l := newLimiter(cfg, c)
	const key = "client"

	l.Fail(key) // tolerated
	l.Fail(key) // locked for a minute

	// Past both the lockout and the eviction window: the entry is safe to drop,
	// and dropping it is what forgets the failure count.
	c.advance(2*time.Minute + time.Second)
	l.Sweep()

	l.Fail(key)
	if got := l.LockedFor(key); got != 0 {
		t.Errorf("LockedFor = %v after Sweep and one failure, want 0: the evicted failure "+
			"count came back and locked the key at once", got)
	}
}

func TestSweepRetainsKeysThatAreNotIdleYet(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxFailures = 1
	cfg.IdleEviction = time.Minute

	c := newClock()
	l := newLimiter(cfg, c)
	const key = "client"

	l.Fail(key) // the one tolerated failure
	c.advance(30 * time.Second)
	l.Sweep() // half the eviction window: too soon to forget anything
	l.Fail(key)

	if got := l.LockedFor(key); got == 0 {
		t.Error("the sweep forgot a failure recorded inside the eviction window, " +
			"handing the attacker a fresh tolerance")
	}
}

func TestConcurrentAllowNeverOverspendsTheBucket(t *testing.T) {
	cfg := baseConfig() // burst 4, and the clock never moves
	c := newClock()
	l := newLimiter(cfg, c)

	const goroutines = 64
	var granted atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, _ := l.Allow("client"); ok {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := granted.Load(); got != int64(cfg.Burst) {
		t.Errorf("%d of %d simultaneous requests were allowed, want exactly Burst (%d); "+
			"the read-modify-write on the bucket is not atomic", got, goroutines, cfg.Burst)
	}
}

func TestConcurrentFailAllowSucceedAndSweepAreConsistent(t *testing.T) {
	c := newClock()
	l := newLimiter(baseConfig(), c)
	keys := []string{"a", "b", "c", "d"}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(4)
		go func(i int) { defer wg.Done(); l.Fail(keys[i%len(keys)]) }(i)
		go func(i int) { defer wg.Done(); l.Allow(keys[i%len(keys)]) }(i)
		go func(i int) { defer wg.Done(); l.Succeed(keys[i%len(keys)]) }(i)
		go func(i int) {
			defer wg.Done()
			_ = l.LockedFor(keys[i%len(keys)])
			if i%4 == 0 {
				l.Sweep()
			}
			c.advance(time.Second)
		}(i)
	}
	wg.Wait()

	// A negative wait would be read as "not locked" by a caller comparing
	// against zero, so it must never escape.
	for _, key := range keys {
		if got := l.LockedFor(key); got < 0 {
			t.Errorf("LockedFor(%q) = %v after concurrent use", key, got)
		}
	}
}

func TestNewWithNilClockUsesTheWallClock(t *testing.T) {
	l := ratelimit.New(baseConfig(), nil)
	if ok, _ := l.Allow("client"); !ok {
		t.Error("Allow denied the first request of a fresh bucket")
	}
	l.Fail("client")
	if got := l.LockedFor("client"); got != 0 {
		t.Errorf("LockedFor after one tolerated failure = %v, want 0", got)
	}
}

func TestAnUnsetIdleEvictionFallsBackToAnHour(t *testing.T) {
	// A caller that forgets the field must not get "evict everything on the next
	// sweep", which would silently discard live lockouts.
	cfg := baseConfig()
	cfg.IdleEviction = 0
	cfg.MaxFailures = 1
	cfg.LockoutBase = 24 * time.Hour
	cfg.MaxLockout = 48 * time.Hour

	c := newClock()
	l := newLimiter(cfg, c)
	l.Fail("attacker")
	l.Fail("attacker")

	c.advance(30 * time.Minute) // half of the one-hour fallback
	l.Sweep()

	if got := l.LockedFor("attacker"); got == 0 {
		t.Error("the sweep dropped a key after the fallback eviction window, not the field's zero value")
	}
}

func TestAnImpossibleRefillRateStillReportsAUsableWait(t *testing.T) {
	// Config validation keeps RequestsPerSecond positive, so this is defensive:
	// the wait for one token divides by the rate, and the result must never come
	// back as zero or negative, which a caller reads as "retry immediately".
	cfg := baseConfig()
	cfg.PerSecond = 0

	c := newClock()
	l := newLimiter(cfg, c)
	for i := 0; i < cfg.Burst; i++ {
		if ok, _ := l.Allow("client"); !ok {
			t.Fatalf("Allow %d was denied while the bucket still held tokens", i+1)
		}
	}

	ok, retry := l.Allow("client")
	if ok {
		t.Fatal("Allow succeeded with an empty bucket and a zero refill rate")
	}
	if retry <= 0 {
		t.Errorf("retryAfter = %v with a zero refill rate, want a positive wait", retry)
	}
}
