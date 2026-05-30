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
	// 48 ticks/cycle (24h / 30m). 240 watches → 5/tick (within bounds).
	if n := s.watchBatchSize(240); n != 5 {
		t.Errorf("batch(240) = %d, want 5", n)
	}
	// Tiny list still gets at least the min.
	if n := s.watchBatchSize(1); n < watchMinBatch {
		t.Errorf("batch(1) = %d, want >= %d", n, watchMinBatch)
	}
	// Huge list is capped.
	if n := s.watchBatchSize(100000); n != watchMaxBatch {
		t.Errorf("batch(huge) = %d, want %d (capped)", n, watchMaxBatch)
	}
}
