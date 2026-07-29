package api

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/ordureconnoisseur/forager/internal/paniclog"
)

// getDiag returns the diagnostics bundle: one JSON blob a tester can paste
// into a bug report that answers the first ten questions without a
// back-and-forth — versions, which sections are configured and where each
// value came from, client reachability, poller health, grab totals,
// journal tallies, and the last recovered panic with its stack.
//
// Redaction: config values go through the same masked field map as
// GET /config (secrets are set/unset flags, never values). The route is
// authenticated like every data route — paths and URLs are fine to include
// here and are exactly what setup bugs hinge on.
func (s *Server) getDiag(w http.ResponseWriter, r *http.Request) {
	bundle := map[string]any{
		"version":   s.version,
		"goVersion": runtime.Version(),
		"os":        runtime.GOOS,
		"arch":      runtime.GOARCH,
		"uptimeSec": int64(time.Since(s.startedAt).Seconds()),
		"now":       time.Now().UTC().Format(time.RFC3339),
		"config":    s.configFields(),
	}

	// Client reachability, same source as /healthz: the background prober.
	clients := map[string]any{
		"stashConfigured":    s.pool.Stash() != nil,
		"stashdbConfigured":  s.pool.StashDB() != nil,
		"prowlarrConfigured": s.pool.Prowlarr() != nil,
		"qbitConfigured":     s.pool.Qbit() != nil,
		"sabConfigured":      s.pool.Sab() != nil,
		"placerConfigured":   s.pool.Placer().Configured(),
	}
	// Which Stash this is talking to. forage tracks current Stash rather
	// than supporting a version range, so the single most useful thing a
	// bug report can carry is which version it actually ran against —
	// otherwise "works for me" and "broken" differ by an unknown.
	// Best-effort and tightly bounded: /diag must stay answerable when
	// Stash is exactly the thing that is wrong.
	if sc := s.pool.Stash(); sc != nil {
		vctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		if v, verr := sc.Version(vctx); verr == nil && v != "" {
			clients["stashVersion"] = v
		} else if verr != nil {
			clients["stashVersionErr"] = verr.Error()
		}
		cancel()
	}

	if h := s.pool.QbitHealth(); h.Probed {
		clients["qbitOk"] = h.OK
		if !h.OK {
			clients["qbitErr"] = h.Err
		}
	}
	if h := s.pool.SabHealth(); h.Probed {
		clients["sabOk"] = h.OK
		if !h.OK {
			clients["sabErr"] = h.Err
		}
	}
	bundle["clients"] = clients

	if s.pollerHealth != nil {
		bundle["poller"] = s.pollerHealth()
	}
	if totals, err := s.grabs.Totals(r.Context()); err == nil {
		bundle["grabTotals"] = totals
	}
	if all, err := s.grabs.CountDestructionsByOutcome(r.Context(), 0); err == nil {
		day, _ := s.grabs.CountDestructionsByOutcome(r.Context(), time.Now().Add(-24*time.Hour).Unix())
		bundle["destructions"] = map[string]any{"total": all, "last24h": day}
	}
	// Full panic entry, stack included — this is the authenticated bundle,
	// and the stack is the whole point of persisting it.
	if pe := paniclog.Last(s.db); pe != nil {
		bundle["lastPanic"] = pe
	}
	writeJSON(w, http.StatusOK, bundle)
}
