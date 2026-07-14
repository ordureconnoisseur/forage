package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Global dead-release memory: releases whose grabs failed for a
// CONTENT-side reason (missing articles, unrepairable, corrupt,
// libtorrent-declined) are dead everywhere, not just on the watch that
// tried them. The per-watch ignored_urls list can't carry that
// knowledge across scenes (the same dead pack release failed on two
// different scenes' watches, and a scene-releases Grab click consults
// no watch at all), so auto-pickers skip this set and the interactive
// release list badges it, letting the user see "this exact release
// already died 8 times" before clicking Grab a 9th.
//
// Keyed by title|indexer|size, lowercased: Prowlarr download URLs
// rotate between searches, but a release's identity holds steady in
// its name + source + size. A same-named release on a DIFFERENT
// indexer (or at a different size) is a different object with its own
// chances, and deliberately does not match.
const deadReleasesTTL = 60 * time.Second

type deadRelease struct {
	Count      int
	LastReason string
}

type deadReleaseCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	set       map[string]deadRelease
}

func deadReleaseKey(title, indexer string, size int64) string {
	return strings.ToLower(strings.TrimSpace(title)) + "|" +
		strings.ToLower(strings.TrimSpace(indexer)) + "|" + fmt.Sprint(size)
}

// contentDeadReleases returns the memoised dead set. Best-effort: on a
// listing error it serves the previous set (or empty), never blocking a
// caller.
func (s *Server) contentDeadReleases(ctx context.Context) map[string]deadRelease {
	if s.grabs == nil {
		return map[string]deadRelease{} // pared-down test servers; nil-safe like failGrab
	}
	s.deadReleases.mu.Lock()
	defer s.deadReleases.mu.Unlock()
	if s.deadReleases.set != nil && time.Since(s.deadReleases.fetchedAt) < deadReleasesTTL {
		return s.deadReleases.set
	}
	failed, err := s.grabs.List(ctx, "failed", "", 500, 0)
	if err != nil {
		if s.deadReleases.set == nil {
			return map[string]deadRelease{}
		}
		return s.deadReleases.set
	}
	set := map[string]deadRelease{}
	for _, g := range failed {
		if !contentDeadReason(g.Reason) {
			continue
		}
		k := deadReleaseKey(g.ReleaseTitle, g.ReleaseIndexer, g.ReleaseSize)
		e := set[k]
		e.Count++
		e.LastReason = g.Reason
		set[k] = e
	}
	s.deadReleases.set = set
	s.deadReleases.fetchedAt = time.Now()
	return set
}
