package clientpool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/config"
)

// ── probeState unit tests (no network) ─────────────────────────────

// A single transient failure must not flip the verdict: it can be a
// false alarm (probe budget expiring while queued behind a slow qBit
// login). Two in a row confirm.
func TestProbeStateTransientNeedsTwoFails(t *testing.T) {
	var s probeState
	transient := fmt.Errorf("dial tcp: connection refused (%w)", clienterr.ErrTransient)

	s.apply(transient)
	if got := s.snapshot(); got.Probed {
		t.Fatalf("after 1 transient failure: want Probed=false (optimistic), got %+v", got)
	}
	s.apply(transient)
	got := s.snapshot()
	if !got.Probed || got.OK {
		t.Fatalf("after 2 transient failures: want Probed=true OK=false, got %+v", got)
	}
	if got.Err == "" {
		t.Fatal("confirmed failure must carry a non-empty Err")
	}
}

// A success resets the failure streak entirely, so an OK verdict
// interleaved between transient failures keeps the client reachable.
func TestProbeStateSuccessResetsStreak(t *testing.T) {
	var s probeState
	transient := fmt.Errorf("timeout (%w)", clienterr.ErrTransient)

	s.apply(transient)
	s.apply(nil)
	got := s.snapshot()
	if !got.Probed || !got.OK || got.Err != "" {
		t.Fatalf("after failure then success: want Probed=true OK=true Err=\"\", got %+v", got)
	}
	// The streak restarted: one more failure stays optimistic-OK.
	s.apply(transient)
	if got := s.snapshot(); !got.OK {
		t.Fatalf("single failure after a success flipped the verdict: %+v", got)
	}
}

// An auth rejection is deterministic, not a blip: it confirms
// immediately and suppresses re-probing for the backoff window (so bad
// credentials don't stack failed logins into a qBit IP ban).
func TestProbeStateRejectedConfirmsImmediatelyAndBacksOff(t *testing.T) {
	var s probeState
	rejected := fmt.Errorf("qbit login refused: Fails. (%w)", clienterr.ErrRejected)

	s.apply(rejected)
	got := s.snapshot()
	if !got.Probed || got.OK {
		t.Fatalf("after rejection: want Probed=true OK=false, got %+v", got)
	}
	if s.shouldProbe(time.Now()) {
		t.Fatal("rejected verdict must suppress probing within the backoff window")
	}
	if !s.shouldProbe(time.Now().Add(healthRejectedBackoff)) {
		t.Fatal("probing must resume once the backoff has elapsed")
	}
}

// A transient failure never enters the rejected backoff: outages keep
// probing every round so recovery is noticed within one interval.
func TestProbeStateTransientKeepsProbing(t *testing.T) {
	var s probeState
	s.apply(fmt.Errorf("refused (%w)", clienterr.ErrTransient))
	s.apply(fmt.Errorf("refused (%w)", clienterr.ErrTransient))
	if !s.shouldProbe(time.Now()) {
		t.Fatal("transient failures must not suppress probing")
	}
}

// reset returns to optimistic-unprobed, the state a freshly swapped-in
// client must start from.
func TestProbeStateReset(t *testing.T) {
	var s probeState
	s.apply(fmt.Errorf("nope (%w)", clienterr.ErrRejected))
	s.reset()
	got := s.snapshot()
	if got.Probed || got.Err != "" {
		t.Fatalf("after reset: want zero state, got %+v", got)
	}
	if !s.shouldProbe(time.Now()) {
		t.Fatal("reset state must probe immediately")
	}
}

// Pathological error with empty text still yields a displayable Err.
func TestProbeStateEmptyErrorText(t *testing.T) {
	var s probeState
	s.apply(emptyErr{})
	s.apply(emptyErr{})
	if got := s.snapshot(); got.Err != "unreachable" {
		t.Fatalf("empty error text: want Err=\"unreachable\", got %+v", got)
	}
}

type emptyErr struct{}

func (emptyErr) Error() string { return "" }

// ── probeHealthRound integration (httptest-backed clients) ─────────

// qbitConfig points the pool's qBit client at url. No username, so
// Login is a no-op and Version's GET is the whole probe.
func qbitConfig(url string) config.Config {
	return config.Config{QbitURL: url}
}

func TestProbeRoundHealthyQbit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v5.1.4"))
	}))
	defer srv.Close()

	p := New()
	p.Reload(qbitConfig(srv.URL))
	p.probeHealthRound(context.Background())

	got := p.QbitHealth()
	if !got.Probed || !got.OK {
		t.Fatalf("healthy qbit: want Probed=true OK=true, got %+v", got)
	}
	if sab := p.SabHealth(); sab.Probed {
		t.Fatalf("unconfigured sab must stay unprobed, got %+v", sab)
	}
}

func TestProbeRoundDeadQbitConfirmsOnSecondRound(t *testing.T) {
	// A server that is immediately closed: connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	p := New()
	p.Reload(qbitConfig(url))

	p.probeHealthRound(context.Background())
	if got := p.QbitHealth(); got.Probed {
		t.Fatalf("first failure must stay optimistic, got %+v", got)
	}
	p.probeHealthRound(context.Background())
	got := p.QbitHealth()
	if !got.Probed || got.OK || got.Err == "" {
		t.Fatalf("second failure must confirm with an error, got %+v", got)
	}
}

func TestProbeRoundRejectedQbitBacksOff(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p := New()
	p.Reload(qbitConfig(srv.URL))

	p.probeHealthRound(context.Background())
	got := p.QbitHealth()
	if !got.Probed || got.OK {
		t.Fatalf("403 must confirm immediately as rejected, got %+v", got)
	}
	before := atomic.LoadInt32(&hits)

	// Within the backoff window another round must not touch the client.
	p.probeHealthRound(context.Background())
	if after := atomic.LoadInt32(&hits); after != before {
		t.Fatalf("backoff violated: %d new requests during rejected backoff", after-before)
	}
}

func TestReloadResetsVerdictAndKicks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p := New()
	p.Reload(qbitConfig(srv.URL))
	p.probeHealthRound(context.Background())
	if got := p.QbitHealth(); !got.Probed {
		t.Fatalf("setup: expected a confirmed verdict, got %+v", got)
	}

	// A config save resets the verdict (the old one described the old
	// client) and leaves a kick pending so RunHealthProbes re-probes at
	// once.
	p.Reload(qbitConfig(srv.URL))
	if got := p.QbitHealth(); got.Probed {
		t.Fatalf("Reload must reset the verdict, got %+v", got)
	}
	select {
	case <-p.healthKick:
	default:
		t.Fatal("Reload must leave a kick pending for the prober")
	}
}

// Regression: at boot (Reload leaves a kick pending, then
// RunHealthProbes starts) exactly ONE immediate round must run. An
// eager round in RunHealthProbes plus the pending kick used to fire two
// back-to-back probes, instantly confirming "unreachable" for a dead
// client and defeating the two-consecutive-failures damping.
func TestRunHealthProbesBootRunsSingleRound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // dead client: every probe fails transiently

	p := New()
	p.Reload(qbitConfig(url)) // leaves a kick pending, as boot does

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.RunHealthProbes(ctx)
		close(done)
	}()
	// Give the kicked round ample time to complete; the 30s ticker
	// cannot fire within this window, so exactly one round runs.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	if got := p.QbitHealth(); got.Probed {
		t.Fatalf("one boot round must stay optimistic (fails=1), got %+v", got)
	}
}

// errors.Is sanity: the sentinel wrapping used across the qbit/sab
// clients survives the fmt.Errorf chains apply depends on.
func TestClienterrClassificationReachesApply(t *testing.T) {
	err := fmt.Errorf("login: %w", fmt.Errorf("qbit login refused: Fails. (%w)", clienterr.ErrRejected))
	if !errors.Is(err, clienterr.ErrRejected) {
		t.Fatal("wrapped rejection must classify as ErrRejected")
	}
}
