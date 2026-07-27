package stashdb

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

// pacer is the request budget for StashDB — the one upstream every forage
// instance shares with the rest of the community. Two mechanisms, both
// applied inside Client.do so every query path (cache refresh, trending,
// subscriptions, watch loop, identify) is covered without opting in:
//
//   - a token bucket bounds the steady-state rate: fan-outs over a big
//     library trickle instead of bursting, while a small burst allowance
//     keeps interactive one-off queries snappy;
//   - a cool-down backs off when StashDB signals distress: any transient
//     failure (5xx, dropped connection, timeout) doubles the pause between
//     requests, a 429 jumps straight to a long one, and the first success
//     clears it. Caller-cancelled contexts are not distress and don't
//     count.
//
// The zero value is not usable; newPacer picks the defaults.
type pacer struct {
	mu     sync.Mutex
	tokens float64   // current bucket fill, capped at burst
	burst  float64   // bucket capacity
	rate   float64   // refill per second (steady-state requests/sec)
	last   time.Time // last refill accounting

	coolUntil time.Time     // requests wait until this passes
	cool      time.Duration // current back-off length (0 = healthy)
	coolBase  time.Duration // first back-off after a transient failure
	cool429   time.Duration // floor for a 429 back-off
	coolMax   time.Duration // back-off ceiling
}

func newPacer() *pacer {
	return &pacer{
		tokens:   4,
		burst:    4,
		rate:     4,
		last:     time.Now(),
		coolBase: 2 * time.Second,
		cool429:  30 * time.Second,
		coolMax:  5 * time.Minute,
	}
}

// wait blocks until the budget admits one request (or ctx is cancelled).
func (p *pacer) wait(ctx context.Context) error {
	for {
		p.mu.Lock()
		now := time.Now()
		// Refill the bucket for the time elapsed since the last accounting.
		p.tokens += now.Sub(p.last).Seconds() * p.rate
		if p.tokens > p.burst {
			p.tokens = p.burst
		}
		p.last = now

		var d time.Duration
		if cd := p.coolUntil.Sub(now); cd > 0 {
			d = cd
		}
		if d == 0 && p.tokens >= 1 {
			p.tokens--
			p.mu.Unlock()
			return nil
		}
		if need := time.Duration((1 - p.tokens) / p.rate * float64(time.Second)); need > d {
			d = need
		}
		p.mu.Unlock()

		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Re-check under the lock: another waiter may have taken the
			// token this one slept for.
		}
	}
}

// observe feeds a request's outcome back into the budget.
func (p *pacer) observe(err error) {
	// The caller walking away mid-request says nothing about StashDB's
	// health — neither distress nor recovery.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err == nil, !errors.Is(err, clienterr.ErrTransient):
		// Success — or a semantic failure (rejected/not-found), which is
		// still the server answering promptly. Either way it's healthy.
		p.cool = 0
	case clienterr.Code(err) == 429:
		// Explicitly told to slow down: long pause, growing if repeated.
		p.escalate(p.cool429)
	default:
		// 5xx / dropped connection / timeout: short pause, doubling while
		// the failures keep coming.
		p.escalate(p.coolBase)
	}
}

// escalate grows the cool-down: up to the class's floor on the first
// failure, doubling on repeats, capped at coolMax. Callers hold p.mu.
func (p *pacer) escalate(floor time.Duration) {
	if p.cool < floor {
		p.cool = floor
	} else {
		p.cool *= 2
	}
	if p.cool > p.coolMax {
		p.cool = p.coolMax
	}
	p.coolUntil = time.Now().Add(p.cool)
}
