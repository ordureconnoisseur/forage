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

// TestTokenizeCaseSymmetricDigits: an uppercase letter run followed by
// digits must tokenise exactly like its lowercase form. The all-caps
// alternative used to consume one digit of the following run (SNOS233 →
// [snos2 33], S01E02 → [s0 1 e0 2]) so the same episode/code tag on the
// two sides of a match shared zero tokens whenever their casing differed.
func TestTokenizeCaseSymmetricDigits(t *testing.T) {
	cases := map[string][]string{
		"SNOS233": {"snos", "233"},
		"snos233": {"snos", "233"},
		"S01E02":  {"s", "01", "e", "02"},
		"s01e02":  {"s", "01", "e", "02"},
		"USB2":    {"usb", "2"},
		"usb2":    {"usb", "2"},
	}
	for in, want := range cases {
		if got := Tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestTokenizeFullwidthFolds: JAV StashDB titles routinely carry fullwidth
// (compatibility-form) letters and digits. NFD has no compatibility
// decomposition, so ＳＴＡＲＳ－６２９ used to tokenise to fullwidth-only
// tokens (digits silently dropped — Go's \d is ASCII-only) that could never
// match the ASCII release name STARS-629.
func TestTokenizeFullwidthFolds(t *testing.T) {
	got := Tokenize("ＳＴＡＲＳ－６２９")
	want := Tokenize("STARS-629")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tokenize(fullwidth) = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(want, []string{"stars", "629"}) {
		t.Errorf("Tokenize(\"STARS-629\") = %v, want [stars 629]", want)
	}
}

// TestScannerConcatNames: member-rip releases fuse the site name in
// lowercase (momdrips_, nfbusty_), one token that can never match the
// multi-token corpus name [mom drips]. With ConcatNames the scanner also
// indexes the concatenated form, under the same singleton safety rules:
// an ambiguous concatenation (collides with another entity's name) is
// dropped.
func TestScannerConcatNames(t *testing.T) {
	corpus := []Entity{
		{ID: "momdrips", Name: "Mom Drips"},
		{ID: "nfbusty", Name: "NF Busty"},
		// Collision pair: "Team Skeet"'s concat equals an existing
		// single-token canonical of another entity → unsafe, not indexed.
		{ID: "teamskeet", Name: "Team Skeet"},
		{ID: "imposter", Name: "TeamSkeet"}, // tokenizes [team skeet] too; its CONCAT also collides
	}
	sc := NewScanner(corpus, StudioScannerOptions())

	if hits := sc.Match("momdrips_bunny_madison2_full_1080"); len(hits) != 1 || hits[0] != "momdrips" {
		t.Errorf("fused lowercase site name should match via concat alias, got %v", hits)
	}
	if hits := sc.Match("nfbusty_maid_for_me_1920"); len(hits) != 1 || hits[0] != "nfbusty" {
		t.Errorf("nfbusty should match, got %v", hits)
	}
	// The separated form still matches as before.
	if hits := sc.Match("Mom.Drips.26.01.01.Performer"); len(hits) != 1 || hits[0] != "momdrips" {
		t.Errorf("separated form regressed: %v", hits)
	}
	// Ambiguous concat: "teamskeet" is claimable by two entities → no
	// single winner may match the fused form.
	hits := sc.Match("teamskeet_full_1080")
	if len(hits) > 1 {
		t.Errorf("ambiguous concat must not match multiple entities: %v", hits)
	}

	// A concat form must CONSULT ownership without CLAIMING it: when
	// another entity already has the fused string as a real single-token
	// alias, that alias keeps matching (the concat is dropped, not the
	// pre-existing alias).
	sc2 := NewScanner([]Entity{
		{ID: "words", Name: "Net Girl"},
		{ID: "fused", Name: "Other Studio", Aliases: []string{"NETGIRL"}},
	}, StudioScannerOptions())
	if hits := sc2.Match("netgirl.26.04.01.monica"); len(hits) != 1 || hits[0] != "fused" {
		t.Errorf("pre-existing real alias must survive a colliding concat, got %v", hits)
	}

	// Without ConcatNames (performer scanner default) behaviour is unchanged.
	plain := NewScanner([]Entity{{ID: "md", Name: "Mom Drips"}}, DefaultScannerOptions())
	if hits := plain.Match("momdrips_bunny_madison2_full_1080"); len(hits) != 0 {
		t.Errorf("ConcatNames off must not index concat forms, got %v", hits)
	}
}

// TestExtractJAVCodesFullwidth: code identity must survive fullwidth
// titles too — the regex is ASCII, so extraction folds first.
func TestExtractJAVCodesFullwidth(t *testing.T) {
	got := ExtractJAVCodes("ＳＴＡＲＳ－６２９ タイトル")
	if len(got) != 1 || got[0] != "stars-629" {
		t.Errorf("ExtractJAVCodes(fullwidth) = %v, want [stars-629]", got)
	}
}
