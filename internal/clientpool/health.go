package clientpool

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

const (
	// healthProbeInterval is the background prober's cadence. Probing is
	// driven by a ticker owned by the Pool, never by request arrival, so
	// /healthz stays passive and an anonymous caller can't influence how
	// often the daemon talks to its clients.
	healthProbeInterval = 30 * time.Second
	// healthProbeTimeout caps a single probe so a hung client (a VPN blip
	// blackholes packets rather than refusing them) can't stall the round.
	healthProbeTimeout = 5 * time.Second
	// healthRejectedBackoff is how long the prober holds off after an
	// auth-rejected failure (bad credentials, an IP ban). Rejection is
	// deterministic: re-probing sooner would just stack failed login
	// attempts, and qBit bans an IP after a handful of consecutive login
	// failures. Recovery is not delayed in practice, because fixing
	// credentials means a config save, and Reload resets the state and
	// kicks an immediate re-probe.
	healthRejectedBackoff = 5 * time.Minute
	// healthConfirmFails is how many consecutive transient failures it
	// takes to report a client unreachable. A single failed probe can be
	// a false alarm (the probe's 5s budget expiring while queued behind a
	// slow qBit login holding the client's auth mutex), so one failure
	// keeps the previous verdict; two in a row (about a minute) confirm.
	healthConfirmFails = 2
)

// ClientHealth is a point-in-time reachability snapshot for one download
// client, as /healthz reports it.
//
// Probed is false until the prober has confirmed a verdict: callers must
// treat !Probed as "reachable, not yet known" and surface no failure. OK
// and Err are meaningful only when Probed is true; Err is non-empty
// whenever OK is false.
type ClientHealth struct {
	Probed bool
	OK     bool
	Err    string
}

// probeState is the health accumulator for one client slot. It lives in
// the Pool next to the atomic client pointer it describes, so Reload can
// reset it when the client it measured is swapped away.
type probeState struct {
	mu       sync.Mutex
	probed   bool   // a verdict (success or confirmed failure) exists
	ok       bool   // the verdict: reachable
	errMsg   string // last failure text (non-empty whenever probed && !ok)
	fails    int    // consecutive transient failures so far
	rejected bool   // last failure was an auth rejection (backoff applies)
	lastAt   time.Time
}

func (s *probeState) snapshot() ClientHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ClientHealth{Probed: s.probed, OK: s.ok, Err: s.errMsg}
}

// reset returns the slot to the optimistic unprobed state. Called by
// Reload so a verdict about the previous client is never attributed to
// the new one.
func (s *probeState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probed, s.ok, s.errMsg, s.fails, s.rejected = false, false, "", 0, false
	s.lastAt = time.Time{}
}

// shouldProbe reports whether the prober should hit the client this
// round. Only an auth-rejected verdict suppresses probing (see
// healthRejectedBackoff); everything else probes every round.
func (s *probeState) shouldProbe(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.rejected || now.Sub(s.lastAt) >= healthRejectedBackoff
}

// apply folds one probe outcome into the state.
func (s *probeState) apply(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAt = time.Now()
	if err == nil {
		s.probed, s.ok, s.errMsg, s.fails, s.rejected = true, true, "", 0, false
		return
	}
	msg := err.Error()
	if msg == "" {
		msg = "unreachable"
	}
	s.errMsg = msg
	if errors.Is(err, clienterr.ErrRejected) {
		// A rejection (bad creds, ban) is deterministic, not a blip:
		// confirm immediately and enter the backoff.
		s.probed, s.ok, s.rejected = true, false, true
		s.fails = healthConfirmFails
		return
	}
	s.rejected = false
	s.fails++
	if s.fails >= healthConfirmFails {
		s.probed, s.ok = true, false
	}
}

// QbitHealth returns the cached reachability of the configured qBit
// client. Zero value (Probed=false) when unconfigured or not yet probed.
func (p *Pool) QbitHealth() ClientHealth { return p.qbitHealth.snapshot() }

// SabHealth is QbitHealth's SABnzbd counterpart.
func (p *Pool) SabHealth() ClientHealth { return p.sabHealth.snapshot() }

// RunHealthProbes probes the configured download clients on a fixed
// cadence, and immediately after every Reload (via the kick channel), so
// the reachability /healthz reports is current within one interval and a
// config fix is re-verified within seconds of saving. Runs until ctx is
// cancelled; launch it from main like the other background loops.
func (p *Pool) RunHealthProbes(ctx context.Context) {
	ticker := time.NewTicker(healthProbeInterval)
	defer ticker.Stop()
	// No eager first round here: the boot Reload already left a kick
	// pending, and it must be the ONLY immediate round. An eager round
	// plus the pending kick would run two probes back to back at
	// startup, and two instant failures would confirm "unreachable"
	// without the ~30s spacing the two-consecutive-failures damping
	// exists to provide.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeHealthRound(ctx)
		case <-p.healthKick:
			p.probeHealthRound(ctx)
		}
	}
}

// probeHealthRound probes each configured client once. The result is
// discarded if a Reload swapped the client mid-probe: the verdict
// describes a client that no longer exists, and the kick that Reload
// sent will probe its replacement immediately. (A swap landing between
// the identity check and apply still slips a stale verdict in, but the
// kicked round overwrites it within seconds, unlike the unbounded
// misattribution this check prevents.)
func (p *Pool) probeHealthRound(ctx context.Context) {
	now := time.Now()
	if qb := p.Qbit(); qb != nil && p.qbitHealth.shouldProbe(now) {
		pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		_, err := qb.Version(pctx)
		cancel()
		if p.Qbit() == qb {
			p.qbitHealth.apply(err)
		}
	}
	if sb := p.Sab(); sb != nil && p.sabHealth.shouldProbe(now) {
		pctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
		_, err := sb.Version(pctx)
		cancel()
		if p.Sab() == sb {
			p.sabHealth.apply(err)
		}
	}
}
