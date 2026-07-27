package poller

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// ── Chaos harness ───────────────────────────────────────────────────
//
// The lifecycle tests prove the poller against clients that behave. This
// harness proves it against clients that DON'T: every HTTP response from
// the fake Stash/qBit/SAB can randomly become a 500, garbage JSON, a
// truncated body, or a dropped connection, for many seeds.
//
// The invariants asserted are the poller's actual safety contract:
//
//   - every observed status change is a legal transition (transitive
//     closure of the state machine, since one tick can advance several
//     steps);
//   - the grab row never disappears;
//   - ZERO scene destroys — no fault pattern may ever push the poller
//     into deleting anything;
//   - and CONVERGENCE: once the faults stop, the grab must reach
//     'confirmed'. Faults are transport-level, and every transport
//     failure is supposed to be guarded (skip-and-retry, never
//     fail-the-grab) — so a grab that ends anywhere else means a guard
//     has a hole in it.

// chaosMW injects seeded faults into a fraction of responses. Disable()
// ends the storm so convergence can be asserted.
type chaosMW struct {
	mu      sync.Mutex
	rng     *rand.Rand
	prob    float64
	enabled bool
	faults  int
}

func newChaosMW(seed int64, prob float64) *chaosMW {
	return &chaosMW{rng: rand.New(rand.NewSource(seed)), prob: prob, enabled: true}
}

func (c *chaosMW) Disable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = false
}

func (c *chaosMW) Faults() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.faults
}

func (c *chaosMW) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		fire := c.enabled && c.rng.Float64() < c.prob
		var mode int
		if fire {
			mode = c.rng.Intn(4)
			c.faults++
		}
		c.mu.Unlock()
		if !fire {
			next.ServeHTTP(w, r)
			return
		}
		switch mode {
		case 0: // server error
			http.Error(w, "chaos: internal error", http.StatusInternalServerError)
		case 1: // garbage body with a 200 — the nastiest kind
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data": {"this is": not json`)
		case 2: // truncated body
			w.Header().Set("Content-Length", "10000")
			_, _ = io.WriteString(w, `{"data":{`)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case 3: // connection dropped mid-air
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			http.Error(w, "chaos", http.StatusBadGateway)
		}
	})
}

// legalSteps is the grab state machine's DIRECT edges, poller-scoped (the
// API's retry/match/delete paths aren't exercised here). The checker works
// on the transitive closure because one tick can advance several steps —
// e.g. queued → completed → placed within a single tickOnce.
var legalSteps = map[string][]string{
	"queued":       {"downloading", "completed", "failed", "deferred"},
	"deferred":     {"queued", "failed"},
	"downloading":  {"completed", "failed"},
	"completed":    {"placed", "failed"},
	"placed":       {"scanned", "confirmed", "mismatched", "orphaned"},
	"scanned":      {"confirmed", "mismatched", "orphaned"},
	"orphaned":     {"scanned", "confirmed", "mismatched"},
	"tagging":      {"confirmed"},
	"distributing": {"confirmed"},
	"confirmed":    {"mismatched"}, // a late link to a different scene
	"mismatched":   {"confirmed"},  // the user corrected the match
	"failed":       {},
}

// legalClosure expands legalSteps to every multi-step transition a single
// observation window may legally contain.
func legalClosure() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for from := range legalSteps {
		reach := map[string]bool{from: true}
		frontier := []string{from}
		for len(frontier) > 0 {
			next := frontier[0]
			frontier = frontier[1:]
			for _, to := range legalSteps[next] {
				if !reach[to] {
					reach[to] = true
					frontier = append(frontier, to)
				}
			}
		}
		out[from] = reach
	}
	return out
}

// snapshot returns id → status for every grab.
func (r *rig) snapshot(t *testing.T) map[int64]string {
	t.Helper()
	rows, err := r.repo.List(context.Background(), "any", "", 500, 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	out := make(map[int64]string, len(rows))
	for _, g := range rows {
		out[g.ID] = g.Status
	}
	return out
}

// TestChaosLifecycleConvergence runs the canonical single-grab lifecycle
// under transport chaos for many seeds. See the package comment above for
// the invariants; the sharpest is convergence — chaos here is purely
// transport-level, and every transport failure is supposed to be guarded,
// so the grab must still end confirmed once the storm passes.
func TestChaosLifecycleConvergence(t *testing.T) {
	const (
		seeds      = 14
		chaosTicks = 24
		calmTicks  = 8
		prob       = 0.35
	)
	closure := legalClosure()

	for seed := int64(0); seed < seeds; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			mw := newChaosMW(seed, prob)
			r := newRigWith(t, "", mw.wrap)
			ctx := context.Background()

			const (
				hash      = "chaoshash"
				predicted = "scene-chaos-001"
				performer = "Chaos Performer"
				fileName  = "Chaos.Release.1080p.mkv"
			)
			src := r.stageFile(t, fileName)
			id, err := r.repo.Insert(ctx, grabs.Grab{
				ReleaseTitle: "Chaos Release", Client: "qbit", ClientID: hash,
				Category: "forager", Status: "queued",
				PredictedStashDBID: predicted, PerformerName: performer,
				Kind: "single", GrabbedAt: time.Now().Unix(),
			})
			if err != nil {
				t.Fatalf("insert: %v", err)
			}

			placedPath := filepath.Join(r.libRoot, performer, fileName)
			prev := r.snapshot(t)
			tick := func(n int, chaosOn bool) {
				for i := 0; i < n; i++ {
					// The environment progresses on its own clock, chaos or
					// not: the torrent completes a third of the way in, and
					// Stash indexes the placed file once it exists.
					switch {
					case i < chaosTicks/3 && chaosOn:
						r.qbit.set([]qbit.Torrent{{
							Hash: hash, Name: fileName, Category: "forager",
							State: "downloading", Progress: 0.5, ContentPath: src,
						}})
					default:
						r.qbit.set([]qbit.Torrent{{
							Hash: hash, Name: fileName, Category: "forager",
							State: "uploading", Progress: 1, ContentPath: src,
						}})
					}
					if g := r.get(t, id); g.PlacedPath != "" {
						r.stash.set([]fakeScene{{
							id: "1", title: "Chaos", path: placedPath, stashDBID: predicted,
						}})
					}
					// A chaotic tick may legally error; an invariant breach
					// may not hide behind that, so keep ticking regardless.
					_ = r.poller.tickOnce(ctx)

					cur := r.snapshot(t)
					if _, ok := cur[id]; !ok {
						t.Fatalf("grab row disappeared at tick %d", i)
					}
					for gid, was := range prev {
						now, ok := cur[gid]
						if !ok {
							t.Fatalf("grab %d vanished", gid)
						}
						if !closure[was][now] {
							t.Fatalf("illegal transition %q → %q at tick %d", was, now, i)
						}
					}
					if got := r.stash.destroys(); len(got) != 0 {
						t.Fatalf("chaos provoked scene destroys: %v", got)
					}
					prev = cur
				}
			}

			tick(chaosTicks, true)
			mw.Disable()
			tick(calmTicks, false)

			g := r.get(t, id)
			if g.Status != "confirmed" {
				t.Fatalf("seed %d (%d faults injected): status=%q reason=%q — "+
					"transport chaos must never leave a grab anywhere but confirmed",
					seed, mw.Faults(), g.Status, g.Reason)
			}
			if g.ActualStashDBID != predicted {
				t.Fatalf("actual=%q, want %q", g.ActualStashDBID, predicted)
			}
		})
	}
}
