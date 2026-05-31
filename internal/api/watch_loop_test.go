package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/watches"
)

func TestResolutionMatches(t *testing.T) {
	cases := []struct {
		target, relRes string
		want           bool
	}{
		{watches.TargetAny, "1080p", true},
		{watches.TargetAny, "", true}, // any accepts even no-resolution
		{"1080p", "1080p", true},
		{"1080p", "4k", false}, // EXACT: 4k does NOT satisfy 1080p
		{"1080p", "720p", false},
		{"4k", "4k", true},
		{"720p", "720p", true},
		{"1080p", "", false}, // no resolution → doesn't satisfy a specific target
	}
	for _, c := range cases {
		if got := resolutionMatches(c.target, c.relRes); got != c.want {
			t.Errorf("resolutionMatches(%q,%q)=%v want %v", c.target, c.relRes, got, c.want)
		}
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
