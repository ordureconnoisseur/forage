package poller

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// TestPickRecentNameDisambiguation reproduces the real swap: two Yasmina
// Khan grabs fired in the same second, both torrents added to qBit within
// the window. Pure time-proximity coin-flipped and crossed them; the
// name-aware tiebreak must link each grab to the torrent whose name
// actually matches its release title.
func TestPickRecentNameDisambiguation(t *testing.T) {
	const at = 1780168872
	// qBit names a torrent after its content, not the tracker release
	// title — so the BBC release's torrent is named for the BBC file and
	// the Shower release's torrent for the Shower file.
	bbcTorrent := qbit.Torrent{
		Hash: "a3739f7a", Category: "forager", AddedOn: at + 1,
		Name: "YASMINA KHAN - BBC GANGBANG WITH MY HUSBAND AND SARA RETALI.mp4",
	}
	showerTorrent := qbit.Torrent{
		Hash: "14e9a3cd", Category: "forager", AddedOn: at + 2,
		Name: "Frances Bentley, Yasmina Khan, Kazumi - Hardcore Gangbang in the Shower! 2160p",
	}
	ts := []qbit.Torrent{showerTorrent, bbcTorrent} // order shouldn't matter

	bbcGrab := &grabs.Grab{
		ReleaseTitle: "[OnlyFans.com / ManyVids.com] Yasmina Khan & Sara Retali - BBC GangBang With My Husband and Sara Retali [2025]",
		GrabbedAt:    at, Category: "forager",
	}
	showerGrab := &grabs.Grab{
		ReleaseTitle: "Frances Bentley, Yasmina Khan, Kazumi - Hardcore Gangbang in the Shower! 2160p",
		GrabbedAt:    at, Category: "forager",
	}

	claimed := map[string]bool{}
	gotBBC := pickRecent(ts, bbcGrab, claimed)
	if gotBBC == nil || gotBBC.Hash != bbcTorrent.Hash {
		t.Fatalf("BBC grab linked to %v, want %s", gotBBC, bbcTorrent.Hash)
	}
	claimed[gotBBC.Hash] = true
	gotShower := pickRecent(ts, showerGrab, claimed)
	if gotShower == nil || gotShower.Hash != showerTorrent.Hash {
		t.Fatalf("Shower grab linked to %v, want %s", gotShower, showerTorrent.Hash)
	}
}

// TestPickRecentSingleCandidate keeps the trivial path intact: one torrent
// in the window links regardless of name.
func TestPickRecentSingleCandidate(t *testing.T) {
	ts := []qbit.Torrent{{Hash: "deadbeef", Category: "forager", AddedOn: 100, Name: "whatever"}}
	g := &grabs.Grab{ReleaseTitle: "totally different", GrabbedAt: 100, Category: "forager"}
	if got := pickRecent(ts, g, map[string]bool{}); got == nil || got.Hash != "deadbeef" {
		t.Fatalf("single candidate not linked: %v", got)
	}
}

// TestPackScanCoverageOK guards the floor that stops a pack confirming
// against a half-scanned directory after a restart re-seeds the
// in-memory settle window at a partial count.
func TestPackScanCoverageOK(t *testing.T) {
	cases := []struct {
		name           string
		found, expected int
		want           bool
	}{
		{"unknown expected count → no floor", 1, 0, true},
		{"negative expected treated as unknown", 5, -1, true},
		{"fully indexed", 126, 126, true},
		{"exactly at floor (80%)", 80, 100, true},
		{"just over floor", 81, 100, true},
		{"just under floor", 79, 100, false},
		{"restart mid-scan: 400 of 1314", 400, 1314, false},
		{"nearly complete 1300 of 1314", 1300, 1314, true},
		{"over-count (title overstated, all real files in)", 130, 126, true},
		{"zero found, real expected", 0, 50, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := packScanCoverageOK(c.found, c.expected); got != c.want {
				t.Errorf("packScanCoverageOK(%d, %d) = %v, want %v",
					c.found, c.expected, got, c.want)
			}
		})
	}
}
