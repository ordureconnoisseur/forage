package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func TestExcludedTagFilter(t *testing.T) {
	// Set build is case-insensitive and trims; empty list → nil (no filter).
	if excludedTagSet(nil) != nil {
		t.Fatal("empty list should yield nil set")
	}
	set := excludedTagSet([]string{"  Compilation ", "Music Video", ""})
	if len(set) != 2 || !set["compilation"] || !set["music video"] {
		t.Fatalf("unexpected set: %v", set)
	}

	scene := func(tags ...string) stashdb.Scene { return stashdb.Scene{Tags: tags} }
	cases := []struct {
		name string
		sc   stashdb.Scene
		want bool
	}{
		{"exact match", scene("Compilation"), true},
		{"case-insensitive match", scene("COMPILATION"), true},
		{"one of several", scene("Anal", "Music Video", "Brunette"), true},
		{"no match", scene("Anal", "Brunette"), false},
		{"no tags", scene(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sceneHasExcludedTag(c.sc, set); got != c.want {
				t.Errorf("sceneHasExcludedTag(%v) = %v, want %v", c.sc.Tags, got, c.want)
			}
		})
	}
}
