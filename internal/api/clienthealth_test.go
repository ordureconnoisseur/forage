package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitProbed spins until the async refresh has completed (probed flips true)
// or the deadline elapses, so tests don't race the refresh goroutine.
func waitProbed(t *testing.T, h *clientHealth) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		done := h.probed && !h.probing
		h.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("probe did not complete within deadline")
}

func TestClientHealthProbesThenReportsFailure(t *testing.T) {
	var h clientHealth
	// Cold cache: first snapshot reports optimistic (not yet probed) and kicks
	// a refresh in the background.
	probed, ok, _ := h.snapshot(func(context.Context) error {
		return errors.New("dial tcp: connection refused")
	})
	if probed {
		t.Fatalf("first snapshot should report probed=false, got true")
	}
	if !ok {
		// ok is meaningless while !probed, but the zero value is false; the
		// caller gates on probed, so this is fine. Nothing to assert here.
	}

	waitProbed(t, &h)

	// After the probe lands, a subsequent read reports the confirmed failure.
	probed, ok, msg := h.snapshot(func(context.Context) error { return nil })
	if !probed || ok {
		t.Fatalf("after failed probe: want probed=true ok=false, got probed=%v ok=%v", probed, ok)
	}
	if msg == "" {
		t.Fatal("expected non-empty error message after a failed probe")
	}
}

func TestClientHealthFreshCacheDoesNotReprobe(t *testing.T) {
	var h clientHealth
	var calls int32

	// First snapshot triggers exactly one probe.
	h.snapshot(func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	waitProbed(t, &h)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("want 1 probe after first snapshot, got %d", got)
	}

	// A second read well within the TTL must serve the cache, not re-probe.
	probed, ok, _ := h.snapshot(func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if !probed || !ok {
		t.Fatalf("want cached probed=true ok=true, got probed=%v ok=%v", probed, ok)
	}
	// Give any (erroneously) spawned refresh a moment to run before asserting.
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fresh cache re-probed: want 1 probe, got %d", got)
	}
}

func TestClientHealthSingleFlight(t *testing.T) {
	var h clientHealth
	var calls int32
	release := make(chan struct{})

	probe := func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		<-release // hold the probe open so concurrent snapshots see probing=true
		return nil
	}

	// Fire many concurrent cold-cache snapshots; only one refresh must launch.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.snapshot(probe)
		}()
	}
	wg.Wait()

	// The spawned refresh goroutine may not have entered probe yet; wait for
	// it to start, then assert no second probe slipped through.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let any stray refresh race in
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("want single in-flight probe, got %d", got)
	}
	close(release)
	waitProbed(t, &h)
}
