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
