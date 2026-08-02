package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// candFixture builds n ranked candidates with descending confidence.
func candFixture(n int) []matcher.Candidate {
	out := make([]matcher.Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, matcher.Candidate{
			Scene: stashdb.Scene{
				ID:    string(rune('a'+i)) + "-scene",
				Title: "Scene " + string(rune('A'+i)),
				Date:  "2026-01-0" + string(rune('1'+i%9)),
			},
			Confidence: 0.9 - float64(i)/100,
		})
	}
	return out
}

// TestExplainCandidatesAlwaysIncludesTheViewedScene is the whole point of the
// panel: the user opened it because the scene they wanted did not verify. If
// that scene ranked below the display cap and got trimmed, the panel answers
// every question except theirs.
func TestExplainCandidatesAlwaysIncludesTheViewedScene(t *testing.T) {
	cands := candFixture(9)
	target := cands[7].Scene.ID // ranked 8th, past maxExplainCandidates

	got := explainCandidates(cands, target)
	if len(got) != maxExplainCandidates+1 {
		t.Fatalf("got %d rows, expected the %d-row cap plus the viewed scene", len(got), maxExplainCandidates)
	}
	last := got[len(got)-1]
	if last.SceneID != target || !last.IsTarget {
		t.Errorf("viewed scene missing from the table; last row is %q (target=%v)", last.SceneID, last.IsTarget)
	}
	if last.Rank != 8 {
		t.Errorf("viewed scene shown at rank %d, expected its TRUE rank 8; a renumbered row would hide how far it lost by", last.Rank)
	}
	for i := 0; i < maxExplainCandidates; i++ {
		if got[i].Rank != i+1 {
			t.Errorf("row %d reports rank %d", i, got[i].Rank)
		}
	}
}

// TestExplainCandidatesNoDuplicateTarget guards the cheap mistake: appending
// the viewed scene a second time when it already made the cut, which would
// show the same row twice and make a two-scene field look like three.
func TestExplainCandidatesNoDuplicateTarget(t *testing.T) {
	cands := candFixture(3)
	got := explainCandidates(cands, cands[1].Scene.ID)
	if len(got) != 3 {
		t.Fatalf("got %d rows for 3 candidates", len(got))
	}
	targets := 0
	for _, c := range got {
		if c.IsTarget {
			targets++
		}
	}
	if targets != 1 {
		t.Errorf("%d rows marked as the viewed scene, expected exactly 1", targets)
	}
}

// TestMatchExplainReportsForageOverrides covers the disagreement the panel
// would otherwise show: the matcher verifies a performer megapack on the
// shared performer name, forage then refuses it, and without the override in
// the payload the panel would read "verified via strong match" beside an
// unverified badge.
func TestMatchExplainReportsForageOverrides(t *testing.T) {
	cands := []matcher.Candidate{{
		Scene:        stashdb.Scene{ID: "s1", Title: "Poolside Confessions Midnight Rendezvous"},
		Confidence:   0.50,
		TitleOverlap: 0.50,
	}}
	overrides := []explainOverride{{Verdict: "refused", Reason: "the title reads as a multi-scene pack"}}

	me := newMatchExplain(cands, "s1", "Poolside Confessions Midnight Rendezvous",
		"Poolside Confessions Midnight Rendezvous MegaPack 1080p", false, overrides, "")
	if me.Verified {
		t.Errorf("payload reports verified, but the badge says otherwise")
	}
	if me.Path == "" {
		t.Errorf("the matcher's own accepting path is missing, so the panel cannot show what the override overrode")
	}
	if len(me.Overrides) != 1 || me.Overrides[0].Verdict != "refused" {
		t.Errorf("override not carried: %+v", me.Overrides)
	}
}

// TestHoistSharedBlockersStatesTheCommonReasonOnce covers both halves of why
// the hoist moved to the daemon: the sentence must appear once rather than on
// every failing path, and the paths it was lifted from must not keep a copy.
func TestHoistSharedBlockersStatesTheCommonReasonOnce(t *testing.T) {
	const notTop = "the matcher ranks this scene #3 of 10, behind \"Other\" at 0.81"
	gates := []matcher.VerifyGate{
		{Name: "rank-title", Blockers: []string{notTop, "title overlap too low"}},
		{Name: "short-title", Blockers: []string{notTop}},
		{Name: "strong-match", Blockers: []string{notTop, "confidence too low"}},
		{Name: "date-anchor", Blockers: []string{notTop}},
		{Name: "containment", Blockers: []string{"title too short"}},
	}
	shared, out := hoistSharedBlockers(gates)
	if len(shared) != 1 || shared[0] != notTop {
		t.Fatalf("shared = %v, expected exactly the not-top sentence", shared)
	}
	for _, g := range out {
		for _, b := range g.Blockers {
			if b == notTop {
				t.Errorf("gate %q still carries the hoisted sentence", g.Name)
			}
		}
	}
	// A gate whose every reason was hoisted keeps its slot with no blockers;
	// the UI reads that as "stopped only by the reason above". A nil-vs-empty
	// slip here would serialise "blockers":[] on four gates per release.
	for _, g := range out {
		if g.Name == "short-title" && g.Blockers != nil {
			t.Errorf("short-title blockers = %v, expected nil", g.Blockers)
		}
	}
	if len(out) != len(gates) {
		t.Errorf("hoisting dropped a gate: %d in, %d out", len(gates), len(out))
	}
}

// TestHoistSharedBlockersLeavesNarrowOnesAlone is the other side of the rule.
// A reason two paths happen to share is not a general fact about the release,
// and hoisting it prints "which rules out every path below that needs it"
// above something that ruled out two of five.
func TestHoistSharedBlockersLeavesNarrowOnesAlone(t *testing.T) {
	const twice = "confidence too low"
	gates := []matcher.VerifyGate{
		{Name: "rank-title", Blockers: []string{twice}},
		{Name: "short-title", Blockers: []string{twice}},
		{Name: "strong-match", Passed: true},
		{Name: "date-anchor", Blockers: []string{"no date anchor"}},
		{Name: "containment", Blockers: []string{"title too short"}},
	}
	shared, out := hoistSharedBlockers(gates)
	if len(shared) != 0 {
		t.Errorf("hoisted %v from only 2 of 4 failing paths", shared)
	}
	for _, g := range out {
		if g.Name == "rank-title" && len(g.Blockers) != 1 {
			t.Errorf("rank-title lost its reason: %v", g.Blockers)
		}
	}
}

// TestGateLabelsCoverEveryGate guards the split that took the labels out of the
// per-release payload: the response sends a name→label table once, so a gate
// whose name is missing from it renders in the UI as a raw identifier.
func TestGateLabelsCoverEveryGate(t *testing.T) {
	labels := matcher.GateLabels()
	cands := []matcher.Candidate{{
		Scene:      stashdb.Scene{ID: "s1", Title: "Some Scene Title Here"},
		Confidence: 0.2,
	}}
	me := newMatchExplain(cands, "s1", "Some Scene Title Here", "unrelated.release.mp4", false, nil, "")
	if len(me.Gates) == 0 {
		t.Fatal("no gates traced")
	}
	for _, g := range me.Gates {
		if labels[g.Name] == "" {
			t.Errorf("gate %q has no label in the response table", g.Name)
		}
	}
	if len(labels) != len(me.Gates) {
		t.Errorf("label table has %d entries for %d gates", len(labels), len(me.Gates))
	}
}

// TestMatchExplainWithNoCandidates covers the release the matcher could not
// place at all. There is no gate trace to show, so the payload must carry the
// note instead of an empty panel that reads as a bug.
func TestMatchExplainWithNoCandidates(t *testing.T) {
	me := newMatchExplain(nil, "s1", "Some Scene", "unrelated.release.name.mp4", false, nil,
		"the matcher retrieved no candidate scenes for this release name")
	if me.Position.Found || me.Position.Candidates != 0 {
		t.Errorf("position claims a field that does not exist: %+v", me.Position)
	}
	if len(me.Gates) != 0 || len(me.Candidates) != 0 {
		t.Errorf("expected an empty trace, got %d gates and %d candidates", len(me.Gates), len(me.Candidates))
	}
	if me.Note == "" {
		t.Errorf("no note, so the panel would render blank")
	}
}
