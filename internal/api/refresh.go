package api

import (
	"context"
	"net/http"
	"time"

	"github.com/ordureconnoisseur/forager/internal/cache"
)

// postRefresh runs performer + studio refreshes synchronously. Both
// pulls are fast enough for typical Stash libraries that we don't need
// to surface a queue. A mutex prevents overlapping invocations from
// hammering Stash; the second caller gets a 409 instead of queueing.
func (s *Server) postRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.refreshMu.TryLock() {
		writeErr(w, http.StatusConflict, "refresh already in progress")
		return
	}
	defer s.refreshMu.Unlock()

	stashC := s.pool.Stash()
	if stashC == nil {
		writeErr(w, http.StatusServiceUnavailable, "stash not configured (see Settings)")
		return
	}

	// Detach from the request context so a client disconnect mid-refresh
	// doesn't abort the in-flight upserts.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := cache.RefreshPerformers(ctx, stashC, s.db, s.log.With("op", "performers")); err != nil {
		s.log.Error("performer refresh failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "performer refresh: "+err.Error())
		return
	}
	if err := cache.RefreshStudios(ctx, stashC, s.pool.StashDB(), s.db, s.log.With("op", "studios")); err != nil {
		s.log.Error("studio refresh failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "studio refresh: "+err.Error())
		return
	}

	perfAt, _ := cache.PerformerRefreshedAt(ctx, s.db)
	studAt, _ := cache.StudioRefreshedAt(ctx, s.db)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"performerRefreshedAt": perfAt,
		"studioRefreshedAt":    studAt,
	})
}
