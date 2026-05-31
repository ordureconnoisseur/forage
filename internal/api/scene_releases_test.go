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
