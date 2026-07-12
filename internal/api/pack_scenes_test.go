package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// TestPackNeedle pins the path-scoping fallback the poller and the pack-scenes
// endpoints share: a path-mapped placed dir when a mapping exists, else the
// placed dir's basename, else the client save-name. Getting this wrong either
// over-matches a performer's other scenes or matches nothing.
func TestPackNeedle(t *testing.T) {
	// No placed path → client save-name.
	if got := packNeedle(&grabs.Grab{ClientName: "Some.Pack"}, ""); got != "Some.Pack" {
		t.Errorf("no placed path: needle = %q, want Some.Pack", got)
	}
	// Placed path, no mapping → basename of the placed dir.
	if got := packNeedle(&grabs.Grab{PlacedPath: "/lib/Comatozze/Comatozze Pack"}, ""); got != "Comatozze Pack" {
		t.Errorf("no mapping: needle = %q, want the placed basename", got)
	}
	// Placed path wins over client name.
	g := &grabs.Grab{PlacedPath: "/lib/X/Y", ClientName: "ignored"}
	if got := packNeedle(g, ""); got == "ignored" {
		t.Errorf("placed path must take precedence over client name, got %q", got)
	}
}
