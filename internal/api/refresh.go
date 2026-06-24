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

	if err := cache.RefreshPerformers(ctx, stashC, s.pool.StashDB(), s.db, s.log.With("op", "performers")); err != nil {
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

// postRefreshPerformers refreshes ONLY the performer cache — the fast pull,
// without the studio sweep. Backs the inline "refresh" affordance on the
// grab performer-reassign search, so a just-created Stash performer shows up
// immediately instead of waiting for the 6h cache tick. Shares refreshMu
// with postRefresh so the two can't run concurrently.
func (s *Server) postRefreshPerformers(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := cache.RefreshPerformers(ctx, stashC, s.pool.StashDB(), s.db, s.log.With("op", "performers")); err != nil {
		s.log.Error("performer refresh failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "performer refresh: "+err.Error())
		return
	}
	perfAt, _ := cache.PerformerRefreshedAt(ctx, s.db)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "performerRefreshedAt": perfAt})
}

// postRefreshStudios re-enumerates the local studio list (name, aliases,
// favorite, scene_count). Like the performer refresh, it does NOT recompute
// the heavy per-studio StashDB aggregates — those land on the 12h ticker via
// RefreshStudioCache.
func (s *Server) postRefreshStudios(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := cache.RefreshStudios(ctx, stashC, s.pool.StashDB(), s.db, s.log.With("op", "studios")); err != nil {
		s.log.Error("studio refresh failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "studio refresh: "+err.Error())
		return
	}
	studAt, _ := cache.StudioRefreshedAt(ctx, s.db)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "studioRefreshedAt": studAt})
}

// postRefreshScenes forces a scene-cache sync (delta, or a full reconcile when
// due) — the StashDB pass that drives the persistent scene cache, the
// performer/studio aggregates and Discover. Runs DETACHED in the background
// (a full reconcile can take minutes) and returns immediately; a TryLock keeps
// two from overlapping.
func (s *Server) postRefreshScenes(w http.ResponseWriter, r *http.Request) {
	if !s.sceneSyncMu.TryLock() {
		writeErr(w, http.StatusConflict, "a scene sync is already running")
		return
	}
	stashC := s.pool.Stash()
	stashDBC := s.pool.StashDB()
	if stashC == nil || stashDBC == nil {
		s.sceneSyncMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "stash and stashdb must be configured (see Settings)")
		return
	}
	go func() {
		defer s.sceneSyncMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := cache.SyncStashDBCounts(ctx, stashC, stashDBC, s.db, s.log.With("op", "count-sync")); err != nil {
			s.log.Error("manual count sync failed", "err", err)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}
