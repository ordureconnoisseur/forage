package api

import (
	"context"
	"sync"
	"time"
)

// clientHealthTTL bounds how stale a cached reachability result may be before
// the next /healthz read triggers a background refresh. clientProbeTimeout
// caps a single probe so a hung or black-holed client can't tie up the
// refresh goroutine (a VPN blip drops packets rather than refusing them, so
// without a timeout the probe would hang until the OS TCP timeout).
const (
	clientHealthTTL    = 20 * time.Second
	clientProbeTimeout = 5 * time.Second
)

// clientHealth caches an active reachability probe for one download client
// (qBit or SAB) so /healthz can report real reachability, not just config
// presence.
//
// /healthz is unauthenticated and polled by the UI before login, so it must
// never block on a client network call. snapshot returns the last cached
// result immediately and kicks off an async refresh only when the cached
// result has gone stale and no refresh is already running — stale-while-
// revalidate. Until the first probe completes the caller treats the client
// as reachable (optimistic): a banner that appears only after a probe has
// actually confirmed a failure never cries wolf during the brief unknown
// window right after boot or a config swap.
//
// The zero value is ready to use. It holds a sync.Mutex, so it must not be
// copied after first use — it lives as a field on the (pointer-only) Server.
type clientHealth struct {
	mu        sync.Mutex
	probed    bool      // at least one probe has completed
	ok        bool      // the last completed probe succeeded
	errMsg    string    // the last probe's error text (empty when ok)
	checkedAt time.Time // when the last probe completed
	probing   bool      // a refresh goroutine is currently in flight
}

// snapshot returns the current cached reachability and, when the cache is
// stale (or never populated) and no refresh is already in flight, launches a
// single background refresh. It never blocks on the probe itself.
//
// probed is false until the first probe completes; callers must treat
// !probed as "reachable, not yet known" and not surface a failure banner.
// The probe closure performs the actual client call (e.g. Client.Version);
// snapshot supplies it a context bounded by clientProbeTimeout.
func (h *clientHealth) snapshot(probe func(context.Context) error) (probed, ok bool, errMsg string) {
	h.mu.Lock()
	stale := h.checkedAt.IsZero() || time.Since(h.checkedAt) >= clientHealthTTL
	if stale && !h.probing {
		h.probing = true
		go h.refresh(probe)
	}
	probed, ok, errMsg = h.probed, h.ok, h.errMsg
	h.mu.Unlock()
	return probed, ok, errMsg
}

// refresh runs one probe under a bounded context and stores the outcome. It
// is only ever entered with h.probing already set true (by snapshot), so at
// most one refresh runs per client at a time; it clears probing on the way
// out. It touches no database and no shared state beyond h, so a process
// shutdown that abandons it mid-flight is harmless.
func (h *clientHealth) refresh(probe func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), clientProbeTimeout)
	err := probe(ctx)
	cancel()

	h.mu.Lock()
	h.probed = true
	h.ok = err == nil
	if err != nil {
		h.errMsg = err.Error()
	} else {
		h.errMsg = ""
	}
	h.checkedAt = time.Now()
	h.probing = false
	h.mu.Unlock()
}
