package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/prowlarr"
)

func TestReleaseContentKey(t *testing.T) {
	// Same torrent across queries — different whitespace/case shouldn't
	// split it (and the grab URL, which differs, isn't part of the key).
	a := prowlarr.Release{Title: "[BJRAW] Foo 1080p", Indexer: "PornoLab", Size: 951700000, Protocol: "torrent"}
	b := prowlarr.Release{Title: "[BJRAW]  Foo  1080p", Indexer: "pornolab", Size: 951700000, Protocol: "torrent"}
	if releaseContentKey(a) != releaseContentKey(b) {
		t.Errorf("same release should share a content key:\n  %q\n  %q",
			releaseContentKey(a), releaseContentKey(b))
	}
	// A genuinely different release (other size/resolution) must differ.
	c := prowlarr.Release{Title: "[BJRAW] Foo 720p", Indexer: "PornoLab", Size: 500000000, Protocol: "torrent"}
	if releaseContentKey(a) == releaseContentKey(c) {
		t.Error("different size/title should produce a different content key")
	}
}

// TestJavCodeMatches: a bare-code OneJAV title verifies against a
// hyphenated/decorated scene title sharing the same code, and a different
// code doesn't.
func TestJavCodeMatches(t *testing.T) {
	cases := []struct {
		rel, scene string
		want       bool
	}{
		{"OAE302", "OAE-302", true},                              // OneJAV bare vs scene hyphenated
		{"OAE302", "瀬戸環奈 OAE-302 (ボール・ドッド) 2026 WEB-DL", true}, // code inside a decorated title
		{"+++ [FHD] SSIS-858 …", "SSIS-858", true},               // decorated release vs bare scene code
		{"OAE302", "OAE-308", false},                             // different number
		{"OAE302", "SSIS-302", false},                            // different studio prefix
		{"Some WEB-DL 1080p release", "OAE-302", false},          // no code in release
		{"OAE302", "Performer Scene Title", false},               // no code in scene
	}
	for _, c := range cases {
		if got := javCodeMatches(c.rel, c.scene); got != c.want {
			t.Errorf("javCodeMatches(%q, %q) = %v, want %v", c.rel, c.scene, got, c.want)
		}
	}
}
