package stashdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

// fastPacer: real clock, durations small enough that a test measures
// behaviour (blocked vs not) rather than exact timings.
func fastPacer() *pacer {
	return &pacer{
		tokens:   2,
		burst:    2,
		rate:     50, // 20ms per token
		last:     time.Now(),
		coolBase: 80 * time.Millisecond,
		cool429:  300 * time.Millisecond,
		coolMax:  time.Second,
	}
}

func TestPacerBurstThenThrottle(t *testing.T) {
	p := fastPacer()
	ctx := context.Background()

	// The burst is admitted immediately…
	start := time.Now()
	for i := 0; i < 2; i++ {
		if err := p.wait(ctx); err != nil {
			t.Fatalf("burst wait %d: %v", i, err)
		}
	}
	if d := time.Since(start); d > 15*time.Millisecond {
		t.Errorf("burst took %v, want ~instant", d)
	}
	// …and the next request has to wait for a refill.
	start = time.Now()
	if err := p.wait(ctx); err != nil {
		t.Fatalf("throttled wait: %v", err)
	}
	if d := time.Since(start); d < 10*time.Millisecond {
		t.Errorf("post-burst wait took %v, want a refill delay", d)
	}
}

func TestPacerBackoffAndRecovery(t *testing.T) {
	p := fastPacer()
	ctx := context.Background()

	// A transient failure opens a cool-down that blocks the next request.
	p.observe(clienterr.Transport("stashdb graphql", errors.New("connection reset")))
	start := time.Now()
	if err := p.wait(ctx); err != nil {
		t.Fatalf("wait during cool-down: %v", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Errorf("cooled wait took %v, want >=~80ms", d)
	}

	// Success clears the back-off entirely: with tokens available the next
	// request is immediate again.
	p.observe(nil)
	p.mu.Lock()
	p.tokens = 1 // make the assertion about the cool-down, not the bucket
	p.mu.Unlock()
	start = time.Now()
	if err := p.wait(ctx); err != nil {
		t.Fatalf("wait after recovery: %v", err)
	}
	if d := time.Since(start); d > 15*time.Millisecond {
		t.Errorf("post-recovery wait took %v, want ~instant", d)
	}
}

func TestPacerBackoffEscalation(t *testing.T) {
	p := fastPacer()
	fail := clienterr.Transport("stashdb graphql", errors.New("boom"))

	p.observe(fail)
	first := p.cool
	p.observe(fail)
	second := p.cool
	if second != 2*first {
		t.Errorf("cool after two failures = %v, want %v (doubled)", second, 2*first)
	}
	// A 429 jumps to its floor rather than doubling from a small base.
	p.observe(clienterr.Status("stashdb graphql", 429, nil))
	if p.cool < p.cool429 {
		t.Errorf("cool after 429 = %v, want >= %v", p.cool, p.cool429)
	}
	// The ceiling holds.
	for i := 0; i < 10; i++ {
		p.observe(fail)
	}
	if p.cool != p.coolMax {
		t.Errorf("cool after sustained failures = %v, want cap %v", p.cool, p.coolMax)
	}
}

func TestPacerIgnoresCallerCancellation(t *testing.T) {
	p := fastPacer()
	p.observe(context.Canceled)
	p.observe(fmt_wrap(context.DeadlineExceeded))
	if p.cool != 0 {
		t.Errorf("caller cancellation opened a cool-down: %v", p.cool)
	}
}

// fmt_wrap mimics a cancelled ctx surfacing through the transport
// classifier, the shape do() actually produces.
func fmt_wrap(err error) error {
	return clienterr.Transport("stashdb budget", err)
}

func TestPacerWaitHonoursContext(t *testing.T) {
	p := fastPacer()
	p.observe(clienterr.Status("stashdb graphql", 429, nil)) // long cool-down
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("cancelled wait returned after %v, want promptly", d)
	}
}
