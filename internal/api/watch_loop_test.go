package api

import (
	"testing"
)

// TestBestWatchMatch pins the no-target preference ranking: surface the
// highest-score VERIFIED, non-rejected, non-ignored, GRABBABLE release —
// regardless of resolution. A dead torrent can't win even with a higher score.
func TestBestWatchMatch(t *testing.T) {
	s := &Server{}
	cands := []sceneRelease{
		{Title: "A 1080p", DownloadURL: "a", Verified: true, Score: 100, Protocol: "usenet"},
		{Title: "B 4k", DownloadURL: "b", Verified: true, Score: 150, Protocol: "usenet"}, // top
		{Title: "C rejected", DownloadURL: "c", Verified: true, Rejected: true, Score: 999},
		{Title: "D unverified", DownloadURL: "d", Verified: false, Score: 999},
		// highest raw score but a dead torrent (0 seeders) — must NOT win.
		{Title: "E 4k dead", DownloadURL: "e", Verified: true, Score: 200, Protocol: "torrent", Seeders: 0},
	}

	best := s.bestWatchMatch(cands, nil)
	if best == nil || best.DownloadURL != "b" {
		t.Fatalf("want B (top grabbable verified by score), got %+v", best)
	}
	// Ignoring B falls through to A (next grabbable verified), NOT the dead E.
	if best := s.bestWatchMatch(cands, []string{"b"}); best == nil || best.DownloadURL != "a" {
		t.Fatalf("with B ignored want A, got %+v", best)
	}
	// Nothing verified → nil (watch stays watching).
	if s.bestWatchMatch([]sceneRelease{{DownloadURL: "x", Verified: false, Score: 999}}, nil) != nil {
		t.Error("no verified release should yield nil")
	}
}

// TestBestWatchMatchConfidenceWithinTier pins the fix for the dated-release
// case: among same-resolution candidates, the surer match (higher confidence —
// e.g. the one carrying the exact scene date) must win even when a fine indexer
// nudge gives a rival a slightly higher preference score.
func TestBestWatchMatchConfidenceWithinTier(t *testing.T) {
	s := &Server{}
	cands := []sceneRelease{
		// Higher score (a 2-point indexer nudge) but a weak match.
		{Title: "Bang Surprise - Angela White 1080p", DownloadURL: "weak",
			Verified: true, Score: 137, Protocol: "usenet", Confidence: 0.45},
		// Lower score but a far surer match (carries the exact scene date).
		{Title: "Bang.26.06.29.Angela.White.XXX.1080p", DownloadURL: "dated",
			Verified: true, Score: 135, Protocol: "usenet", Confidence: 0.90},
	}
	if best := s.bestWatchMatch(cands, nil); best == nil || best.DownloadURL != "dated" {
		t.Fatalf("same-resolution: higher-confidence (dated) must beat higher-score, got %+v", best)
	}
	// But a genuine resolution difference still defers to the user's score: a
	// 4K with lower confidence beats a 1080p only if the score says so.
	cross := []sceneRelease{
		{Title: "Scene 1080p", DownloadURL: "hd", Verified: true, Score: 100, Protocol: "usenet", Confidence: 0.95},
		{Title: "Scene 2160p", DownloadURL: "uhd", Verified: true, Score: 60, Protocol: "usenet", Confidence: 0.50},
	}
	if best := s.bestWatchMatch(cross, nil); best == nil || best.DownloadURL != "hd" {
		t.Fatalf("cross-resolution: user score (1080p>4K) must decide, not confidence, got %+v", best)
	}
}

func TestWatchBatchSize(t *testing.T) {
	s := &Server{}
	// Small lists are fully checked each tick (responsive).
	if n := s.watchBatchSize(1); n != 1 {
		t.Errorf("batch(1) = %d, want 1", n)
	}
	if n := s.watchBatchSize(8); n != 8 {
		t.Errorf("batch(8) = %d, want 8 (all checked while ≤ cap)", n)
	}
	// Larger lists cap at watchMaxBatch and spread across ticks.
	if n := s.watchBatchSize(9); n != watchMaxBatch {
		t.Errorf("batch(9) = %d, want %d (capped)", n, watchMaxBatch)
	}
	if n := s.watchBatchSize(100000); n != watchMaxBatch {
		t.Errorf("batch(huge) = %d, want %d (capped)", n, watchMaxBatch)
	}
}
