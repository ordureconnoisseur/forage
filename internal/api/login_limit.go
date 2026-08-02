package api

import (
	"sync"
	"time"
)

// ── Login throttling ────────────────────────────────────────────────
//
// Before the 2026-08 audit an attacker could post guesses at /login as
// fast as bcrypt would answer them, forever. forage deliberately has no
// account lockout (neither do the *arrs, and a lockout on a single-user
// daemon is a self-inflicted denial of service), so the control is a
// per-client failure budget that refills on its own: after
// loginMaxFailures bad credentials inside loginFailureWindow, further
// attempts from that client are refused with 429 until the window ends.
// Nothing is persisted and nothing is permanent — the daemon is a single
// instance, and the worst case for a legitimate user who fat-fingered ten
// passwords is a five-minute wait.
//
// Only FAILURES are counted and a success clears the client's record, so
// ordinary use never approaches the limit.

const (
	// loginMaxFailures is the failure budget per client per window.
	loginMaxFailures = 10
	// loginFailureWindow is how long that budget takes to refill.
	loginFailureWindow = 5 * time.Minute
	// loginLimiterSweepAt is the tracked-client count that triggers a purge
	// of expired records. The map is naturally bounded by distinct client
	// addresses seen inside one window; the sweep keeps a scan of the
	// internet from parking memory for the full window.
	loginLimiterSweepAt = 1024
)

// loginFailures is one client's running failure count and the instant the
// count (and any block) expires.
type loginFailures struct {
	count   int
	expires time.Time
}

// loginLimiter tracks failed credential checks per client. The zero value
// is ready to use; window/max/now are test seams.
type loginLimiter struct {
	mu      sync.Mutex
	clients map[string]loginFailures

	window time.Duration    // 0 → loginFailureWindow
	max    int              // 0 → loginMaxFailures
	now    func() time.Time // nil → time.Now
}

func (l *loginLimiter) settings() (time.Duration, int, time.Time) {
	w, m, now := l.window, l.max, time.Now
	if w == 0 {
		w = loginFailureWindow
	}
	if m == 0 {
		m = loginMaxFailures
	}
	if l.now != nil {
		now = l.now
	}
	return w, m, now()
}

// blocked reports whether key has spent its failure budget, and how long
// is left on the window if so.
func (l *loginLimiter) blocked(key string) (time.Duration, bool) {
	_, max, now := l.settings()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.clients[key]
	if !ok || e.count < max || !now.Before(e.expires) {
		return 0, false
	}
	return e.expires.Sub(now), true
}

// fail records one failed credential check for key. The window is anchored
// on the first failure rather than extended by each one, so a client that
// keeps hammering still gets its budget back on schedule instead of being
// held out indefinitely.
func (l *loginLimiter) fail(key string) {
	window, _, now := l.settings()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clients == nil {
		l.clients = make(map[string]loginFailures)
	}
	if len(l.clients) >= loginLimiterSweepAt {
		for k, e := range l.clients {
			if !now.Before(e.expires) {
				delete(l.clients, k)
			}
		}
	}
	e, ok := l.clients[key]
	if !ok || !now.Before(e.expires) {
		e = loginFailures{expires: now.Add(window)}
	}
	e.count++
	l.clients[key] = e
}

// succeed clears key's record: proving you hold the credential should not
// leave you one bad keystroke away from a lockout.
func (l *loginLimiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, key)
}
