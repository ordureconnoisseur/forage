package matcher

import (
	"reflect"
	"strings"
	"testing"
)

// TestTokenizeFoldsDiacritics is the regression guard for audit #1: a
// StashDB name with accents must tokenise identically to the same name with
// the accents stripped (the form P2P release titles use). Before folding,
// "Renée" tokenised to [ren, e] (the é dropped, splitting the run) while
// "Renee" gave [renee] — so they never matched.
func TestTokenizeFoldsDiacritics(t *testing.T) {
	pairs := [][2]string{
		{"Renée", "Renee"},
		{"José García", "Jose Garcia"},
		{"Zoë", "Zoe"},
		{"Anaïs", "Anais"},
		{"Chloé Lacroix", "Chloe Lacroix"},
		{"Lucía Núñez", "Lucia Nunez"},
	}
	for _, p := range pairs {
		got := Tokenize(p[0])
		want := Tokenize(p[1])
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q)=%v != Tokenize(%q)=%v", p[0], got, p[1], want)
		}
	}
}

// TestTokenizeAccentedSingleRun confirms an accented name collapses to a
// single token rather than shattering around the accent.
func TestTokenizeAccentedSingleRun(t *testing.T) {
	if got := Tokenize("Renée"); !reflect.DeepEqual(got, []string{"renee"}) {
		t.Errorf("Tokenize(\"Renée\") = %v, want [renee]", got)
	}
}

// TestTokenizeASCIIUnchanged pins that folding + Unicode classes don't alter
// ASCII tokenisation — the JAV all-caps rule and CamelCase split still hold.
func TestTokenizeASCIIUnchanged(t *testing.T) {
	cases := map[string][]string{
		"Bang Bros":     {"bang", "bros"},
		"SNOS-233":      {"snos", "233"},
		"BangBros":      {"bang", "bros"},
		"Scene.1080p":   {"scene", "1080", "p"},
		"Reality Kings": {"reality", "kings"},
	}
	for in, want := range cases {
		if got := Tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestScannerMatchesAccentedPerformer is the end-to-end proof: a performer
// whose StashDB name carries accents is found in a release title that
// stripped them — the recall gap the audit flagged.
func TestScannerMatchesAccentedPerformer(t *testing.T) {
	corpus := []Entity{
		{ID: "p1", Name: "Renée Gaillard"},
		{ID: "p2", Name: "Someone Else"},
	}
	sc := NewScanner(corpus, DefaultScannerOptions())
	hits := sc.Match("Renee.Gaillard.Goes.Hard.XXX.1080p.MP4")
	if len(hits) != 1 || hits[0] != "p1" {
		t.Fatalf("Match = %v, want [p1]", hits)
	}
}

// TestTokenizeCaselessKept confirms caseless-script letters (CJK) survive as
// a whole token instead of being dropped by the old ASCII-only classes.
func TestTokenizeCaselessKept(t *testing.T) {
	got := Tokenize("作品 ABC-123")
	// The CJK run is kept as one token; ASCII code splits as before.
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "作品") {
		t.Errorf("Tokenize dropped the CJK token: %v", got)
	}
}
