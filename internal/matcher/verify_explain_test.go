package matcher

import (
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// TestExplainAgreesWithVerifyOnCorpus is the anti-drift guarantee.
//
// The whole reason the explanation is produced by the verdict function rather
// than by a second implementation is that a badge saying "not verified" beside
// a panel saying "verified via strong match" is worse than no panel. This
// replays every recorded corpus entry against every candidate scene in it and
// fails if the two answers ever differ, ~16,000 comparisons, no network.
//
// WHAT IT DOES NOT PROVE, despite an earlier version of this comment claiming
// it did: that the switch-to-gates refactor is faithful to the code it
// replaced. Both arms here are the SAME implementation. VerifyWith is
// verifyTrace(full=false) and ExplainVerifyWith is verifyTrace(full=true), so a
// condition transcribed wrong out of the old switch is present identically in
// both and this test stays green. What it does cover is the difference between
// the two modes: short-circuiting (checks stop at the first blocker, gates stop
// at the first acceptance) must not change an answer, and the full pass
// evaluates dateAnchored where the fast pass may not.
//
// The faithfulness claim lives in verify_oracle_test.go, which compares against
// a verbatim copy of the pre-refactor VerifyWith.
func TestExplainAgreesWithVerifyOnCorpus(t *testing.T) {
	entries := loadCorpusFixture(t)
	mismatches := 0
	for _, e := range entries {
		cands := e.Cands()
		for _, c := range cands {
			want := VerifyWith(DefaultVerifyConfig, cands, c.Scene.ID, c.Scene.Title, e.Release)
			got := ExplainVerifyWith(DefaultVerifyConfig, cands, c.Scene.ID, c.Scene.Title, e.Release)
			if want.Verified == got.Verified && want.Confidence == got.Confidence {
				continue
			}
			mismatches++
			if mismatches <= 5 {
				t.Errorf("release %q scene %q: Verify says (%v, %.4f), Explain says (%v, %.4f)",
					e.Release, c.Scene.ID, want.Verified, want.Confidence, got.Verified, got.Confidence)
			}
		}
	}
	if mismatches > 0 {
		t.Errorf("%d verdict/explanation disagreements: the panel would contradict the badge it explains", mismatches)
	}
}

// TestExplainNamesTheAcceptingPath checks that a verified release says WHICH
// of the four paths carried it. Without this the panel can only repeat the
// badge, and the user still cannot tell an identity-level match from a
// title-word coincidence that happened to clear a floor.
func TestExplainNamesTheAcceptingPath(t *testing.T) {
	tests := []struct {
		name     string
		cands    []Candidate
		sceneID  string
		title    string
		release  string
		wantPath string
	}{
		{
			// Title carries it: the release names the scene and it ranks first.
			name: "rank-title",
			cands: []Candidate{{
				Scene:        stashdb.Scene{ID: "s1", Title: "Poolside Confessions Midnight Rendezvous"},
				Confidence:   0.50,
				TitleOverlap: 0.50,
			}},
			sceneID:  "s1",
			title:    "Poolside Confessions Midnight Rendezvous",
			release:  "Studio Poolside Confessions Midnight Rendezvous 1080p",
			wantPath: GateRankTitle,
		},
		{
			// Tag-soup release name crushes title overlap, but performer +
			// date + studio corroborate at identity level.
			name: "strong-match",
			cands: []Candidate{{
				Scene:        stashdb.Scene{ID: "s1", Title: "Wrestling Stepmom For Pussy S13:E6"},
				Confidence:   0.87,
				TitleOverlap: 0.12,
			}},
			sceneID:  "s1",
			title:    "Wrestling Stepmom For Pussy S13:E6",
			release:  "[BrattyMILF.com] Asteria Jade [2026-03-26, Brunette, Big Ass, POV, 1080p, SiteRip]",
			wantPath: GateStrongMatch,
		},
		{
			// Not ranked first (never a candidate at all), but the release
			// spells the title out: the one path that does not need rank.
			name: "containment",
			cands: []Candidate{
				{Scene: stashdb.Scene{ID: "other", Title: "Something Else Entirely"}, Confidence: 0.60},
				{Scene: stashdb.Scene{ID: "s1", Title: "Afternoon Hookup By The Lake"}, Confidence: 0.40},
			},
			sceneID:  "s1",
			title:    "Afternoon Hookup By The Lake",
			release:  "Afternoon Hookup By The Lake 2160p",
			wantPath: GateContainment,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := ExplainVerify(tt.cands, tt.sceneID, tt.title, tt.release)
			if !ex.Verified {
				t.Fatalf("expected verified, got refused (gates: %v)", gateSummary(ex))
			}
			if ex.Path != tt.wantPath {
				t.Errorf("verified via %q, expected %q; the panel would name the wrong reason",
					ex.Path, tt.wantPath)
			}
			if ex.PathLabel == "" {
				t.Errorf("accepting path has no human label, so the UI has nothing to show")
			}
		})
	}
}

// TestExplainBlockersQuoteBothNumbers is the legibility contract: a user who
// has never seen VerifyConfig must be able to read a refusal. Every blocker on
// a refused release has to carry the measured value AND the bar it missed, not
// just "too low".
func TestExplainBlockersQuoteBothNumbers(t *testing.T) {
	// Performer-only coincidence: shares a performer, agrees on nothing else.
	cands := []Candidate{{
		Scene:        stashdb.Scene{ID: "s1", Title: "Some Other Scene Entirely"},
		Confidence:   0.46,
		TitleOverlap: 0.05,
	}}
	ex := ExplainVerify(cands, "s1", "Some Other Scene Entirely",
		"Studio - Shared Performer - A Completely Different Title 1080p")
	if ex.Verified {
		t.Fatalf("performer coincidence must not verify")
	}
	if len(ex.Gates) != 5 {
		t.Fatalf("expected all 5 acceptance paths traced, got %d: a refusal must account for every way it could have passed", len(ex.Gates))
	}
	for _, g := range ex.Gates {
		if g.Passed {
			t.Fatalf("gate %q passed on a refused release", g.Name)
		}
		if len(g.Blockers) == 0 {
			t.Errorf("gate %q refused with no reason given", g.Name)
			continue
		}
		for _, b := range g.Blockers {
			// Prose, not a field dump: a blocker that reads "TitleOverlap
			// 0.05" tells the user nothing they could not already see.
			if len(b) < 30 || strings.Contains(b, "cfg.") {
				t.Errorf("gate %q blocker is not a readable explanation: %q", g.Name, b)
			}
		}
	}
	// The numeric refusals must quote the measurement AND the bar, or "too
	// low" is unfalsifiable to someone who does not know the thresholds.
	rank := gateByName(t, ex, GateRankTitle)
	joined := strings.Join(rank.Blockers, " ")
	for _, want := range []string{"0.05", "0.15"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rank-title refusal omits %s (measured overlap / required floor): %q", want, joined)
		}
	}
	// The headline number a flat candidate field turns on.
	if ex.Signals.Rank != 1 || ex.Signals.Candidates != 1 {
		t.Errorf("signals misreport position: rank %d of %d", ex.Signals.Rank, ex.Signals.Candidates)
	}
}

// TestExplainLosingSceneNamesTheWinner covers the question the panel exists to
// answer: the user expected THIS scene, and it lost. The refusal has to name
// what beat it and by how much, or they are back to guessing.
func TestExplainLosingSceneNamesTheWinner(t *testing.T) {
	cands := []Candidate{
		{Scene: stashdb.Scene{ID: "winner", Title: "The Scene That Won"}, Confidence: 0.740},
		{Scene: stashdb.Scene{ID: "expected", Title: "The Scene You Wanted"}, Confidence: 0.727},
		{Scene: stashdb.Scene{ID: "third", Title: "A Third Sibling"}, Confidence: 0.672},
	}
	ex := ExplainVerify(cands, "expected", "The Scene You Wanted", "SomeStudio 20 05 03 Shared Performer 1080p")
	if ex.Verified {
		t.Fatalf("a scene ranked second must not verify through a ranking path")
	}
	if ex.Signals.Rank != 2 {
		t.Errorf("rank reported as %d, expected 2", ex.Signals.Rank)
	}
	if ex.Signals.TopSceneTitle != "The Scene That Won" {
		t.Errorf("winner reported as %q", ex.Signals.TopSceneTitle)
	}
	// The 0.013 separation is the fact that makes this decision look shaky;
	// it has to reach the user.
	if ex.Signals.RunnerUpConf != 0.740 {
		t.Errorf("runner-up confidence reported as %.3f, expected 0.740", ex.Signals.RunnerUpConf)
	}
	var found bool
	for _, g := range ex.Gates {
		for _, b := range g.Blockers {
			if strings.Contains(b, "The Scene That Won") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no blocker names the scene that beat this one: %v", gateSummary(ex))
	}
}

// TestExplainVetoStillShowsWhatWouldHaveCarried guards the BTS case. A
// behind-the-scenes cut shares its main scene's cast, date, studio and title,
// so it scores at identity level and is refused only by the BTS rule. If the
// panel showed an empty gate list the user would think nothing matched, when
// in fact everything did and one rule overruled it.
func TestExplainVetoStillShowsWhatWouldHaveCarried(t *testing.T) {
	title := "BTS - Poolside Confessions"
	cands := []Candidate{{
		Scene:        stashdb.Scene{ID: "s1", Title: title},
		Confidence:   0.88,
		TitleOverlap: 0.45,
	}}
	release := "SomeStudio Poolside Confessions 1080p"
	if VerifyWith(DefaultVerifyConfig, cands, "s1", title, release).Verified {
		t.Fatalf("a BTS scene must not verify against a release that does not say BTS")
	}
	ex := ExplainVerify(cands, "s1", title, release)
	if ex.Verified {
		t.Fatalf("explained verdict disagrees with Verify")
	}
	if ex.Veto == "" {
		t.Fatalf("refused with no veto recorded, so the UI cannot say why")
	}
	if ex.Path != "" {
		t.Errorf("a vetoed release must not claim an accepting path, got %q", ex.Path)
	}
	passed := false
	for _, g := range ex.Gates {
		if g.Passed {
			passed = true
		}
	}
	if !passed {
		t.Errorf("no gate passed, so the trace does not show that only the veto stopped it: %v", gateSummary(ex))
	}
}

// TestExplainCarriesContainmentConfidenceLift guards the one place the verdict
// function rewrites its own confidence: when the release spells the title out,
// containment replaces a weaker weighted score. The explanation reports the
// same number the badge does.
func TestExplainCarriesContainmentConfidenceLift(t *testing.T) {
	title := "Afternoon Hookup By The Lake"
	cands := []Candidate{
		{Scene: stashdb.Scene{ID: "other", Title: "Something Else Entirely"}, Confidence: 0.60},
		{Scene: stashdb.Scene{ID: "s1", Title: title}, Confidence: 0.35},
	}
	release := title + " 2160p"
	want := VerifyWith(DefaultVerifyConfig, cands, "s1", title, release)
	ex := ExplainVerify(cands, "s1", title, release)
	if !want.Verified || ex.Path != GateContainment {
		t.Fatalf("setup no longer exercises containment: verified=%v path=%q", want.Verified, ex.Path)
	}
	if ex.Confidence != want.Confidence {
		t.Errorf("explained confidence %.4f, badge shows %.4f", ex.Confidence, want.Confidence)
	}
	if ex.Confidence <= 0.35 {
		t.Errorf("containment did not lift confidence above the weighted 0.35, got %.4f", ex.Confidence)
	}
}

// gateByName pulls one gate out of a trace, failing if it is missing.
func gateByName(t *testing.T, ex VerifyExplanation, name string) VerifyGate {
	t.Helper()
	for _, g := range ex.Gates {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("gate %q missing from trace: %v", name, gateSummary(ex))
	return VerifyGate{}
}

// gateSummary renders a trace compactly for failure messages.
func gateSummary(ex VerifyExplanation) string {
	var b strings.Builder
	for _, g := range ex.Gates {
		b.WriteString(g.Name)
		if g.Passed {
			b.WriteString("=pass; ")
			continue
		}
		b.WriteString("=[" + strings.Join(g.Blockers, " | ") + "]; ")
	}
	return b.String()
}
