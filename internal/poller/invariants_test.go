package poller

import (
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

// A tick runs the invariant checker and publishes its report.
//
// The wiring is the whole feature. A checker that exists but is never
// scheduled is exactly the weak feedback loop it was written to replace, and
// this is the assertion that would fail if the pass were ever moved below
// tickOnce's nothing-active early return, where an idle library (the one
// state in which a silent inconsistency is the only thing left to find) would
// stop it running at all.
func TestTickRunsInvariantChecker(t *testing.T) {
	p := newTestPoller(t)
	if p.Invariants() != nil {
		t.Fatal("a report exists before any tick has run")
	}
	// No grabs at all: the pass must still run and publish. (This is the
	// early-return case; an empty Active() set is normal on a quiet daemon.)
	if err := p.tickOnce(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	rep := p.Invariants()
	if rep == nil {
		t.Fatal("no invariant report after a tick; the pass did not run")
	}
	if len(rep.Results) == 0 {
		t.Fatal("report carries no results")
	}
	if rep.RanAt == 0 {
		t.Error("report has no run timestamp, so nothing can tell a stale one from a fresh one")
	}
}

// The cadence gate holds: two ticks a second apart run the suite once. The
// bounded checks cost os.Stat per row and a Stash round-trip per row, and
// running them at the 60s tick interval would put that traffic in front of
// real placement work.
func TestInvariantPassRespectsItsCadence(t *testing.T) {
	p := newTestPoller(t)
	if err := p.tickOnce(t.Context()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	first := p.Invariants().RanAt
	p.lastInvariants = time.Now().Add(-invariantInterval / 2)
	if err := p.tickOnce(t.Context()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if got := p.Invariants().RanAt; got != first {
		t.Errorf("ran again %ds into a %s interval", got-first, invariantInterval)
	}
}

// Stash reports one ref per FILE, and a matching-fingerprint re-download is
// filed as a second file on the SAME scene. Counting refs is what made one
// scene look like two in the pack-dedup path; the checker's own question is
// only "zero or not", but the number it is handed should still be scenes.
func TestCountDistinctScenes(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []stash.SceneRef
		want int
	}{
		{"no refs at all is a scene Stash no longer has", nil, 0},
		{"two files on one scene is one scene", []stash.SceneRef{
			{SceneID: "s1", Path: "/a.mp4"}, {SceneID: "s1", Path: "/a.copy.mp4"},
		}, 1},
		{"two scenes carrying the cross-id is two", []stash.SceneRef{
			{SceneID: "s1"}, {SceneID: "s2"},
		}, 2},
		{"a ref with no scene id counts for nothing", []stash.SceneRef{{Path: "/a.mp4"}}, 0},
	} {
		if got := countDistinctScenes(tc.refs); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}
