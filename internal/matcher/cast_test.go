package matcher

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func TestNameInTokens(t *testing.T) {
	rel := Tokenize("[Swallowed.com] Asteria Jade, Samantha Reigns, Lucia Rossi - Asteria, Sam & Lucia Ramp It Up (04.05.2026)")
	cases := []struct {
		name string
		want bool
	}{
		{"Lucia Rossi", true},       // contiguous multi-token
		{"Asteria Jade", true},      // contiguous
		{"Samantha Reigns", true},   // contiguous
		{"Mike Adriano", false},     // not named in this release
		{"Swallowed Reigns", false}, // both tokens present but far apart, not adjacent
		{"Jade", true},              // single token, >=4 chars, present
		{"Sam", false},              // single token <4 chars — too short to trust
		{"", false},                 // empty
	}
	for _, c := range cases {
		if got := nameInTokens(c.name, rel); got != c.want {
			t.Errorf("nameInTokens(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCastInTitle(t *testing.T) {
	scene := stashdb.Scene{
		Performers: []stashdb.ScenePerformer{
			{Name: "Asteria Jade"},
			{Name: "Samantha Reigns"},
			{Name: "Lucia Rossi"},
			{Name: "Mike Adriano"}, // not named in the release title
		},
	}
	rel := Tokenize("[Swallowed.com] Asteria Jade, Samantha Reigns, Lucia Rossi - Asteria, Sam & Lucia Ramp It Up")
	frac, reason := castInTitle(scene, rel)
	// 3 of 4 cast named.
	if frac < 0.74 || frac > 0.76 {
		t.Errorf("castInTitle frac = %.3f, want ~0.75", frac)
	}
	if reason != "cast: 3/4 named" {
		t.Errorf("castInTitle reason = %q, want %q", reason, "cast: 3/4 named")
	}
}

func TestCastInTitleAlias(t *testing.T) {
	// The credited scene alias (As) should match even when the canonical
	// name doesn't appear.
	scene := stashdb.Scene{
		Performers: []stashdb.ScenePerformer{
			{Name: "Jane Canonical", As: "Bunny Stage"},
		},
	}
	rel := Tokenize("Studio - Bunny Stage - Some Title 1080p")
	frac, _ := castInTitle(scene, rel)
	if frac != 1.0 {
		t.Errorf("alias match frac = %.3f, want 1.0", frac)
	}
}

func TestCastInTitleEmpty(t *testing.T) {
	if frac, reason := castInTitle(stashdb.Scene{}, []string{"a", "b"}); frac != 0 || reason != "" {
		t.Errorf("empty cast = (%.2f,%q), want (0,\"\")", frac, reason)
	}
}
