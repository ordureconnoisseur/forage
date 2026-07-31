package matcher

import (
	"path/filepath"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func cand(id, title, date string, conf, overlap float64, cast ...string) Candidate {
	sc := stashdb.Scene{ID: id, Title: title, Date: date}
	for _, n := range cast {
		sc.Performers = append(sc.Performers, stashdb.ScenePerformer{Name: n})
	}
	return Candidate{Scene: sc, Confidence: conf, TitleOverlap: overlap}
}

// A replayed candidate set must verify identically to the live one, or every
// offline experiment is measuring a different verifier from the one that ships.
func TestReplayRoundTripsThroughVerify(t *testing.T) {
	live := []Candidate{
		cand("a", "Chloe Cherry Seeks Out Massage Therapy", "2017-10-17", 0.72, 0.31, "Chloe Cherry", "Jerry Kovak"),
		cand("b", "Slim Teen Chloe Cherry Sucks and Fucks", "2016-09-06", 0.68, 0.11, "Chloe Cherry"),
	}
	live[1].DateFarOff = true
	release := "Lethal.Hardcore.Chloe.Cherry.Seeks.Out.Massage.Therapy.1080p"

	rec := RecordCandidates(release, "a", live)
	got := rec.Cands()
	if len(got) != len(live) {
		t.Fatalf("recorded %d candidates, want %d", len(got), len(live))
	}
	for i := range live {
		want := VerifyWith(DefaultVerifyConfig, live, live[i].Scene.ID, live[i].Scene.Title, release)
		have := VerifyWith(DefaultVerifyConfig, got, got[i].Scene.ID, got[i].Scene.Title, release)
		if want != have {
			t.Errorf("candidate %s: live %+v, replayed %+v", live[i].Scene.ID, want, have)
		}
	}
	// The fields Verify actually reads must survive the round trip.
	if got[1].DateFarOff != true || got[0].TitleOverlap != 0.31 ||
		len(got[0].Scene.Performers) != 2 || got[0].Scene.Date != "2017-10-17" {
		t.Errorf("lost a field Verify reads: %+v", got[0])
	}
}

// The config refactor must not have moved the shipped decision. Verify and
// VerifyWith(DefaultVerifyConfig) are the same function or the default is a
// lie.
func TestVerifyMatchesDefaultConfig(t *testing.T) {
	cands := []Candidate{
		cand("a", "The Family Secret pt. 2", "2020-08-01", 0.80, 0.42, "Kenzie Reeves"),
		cand("b", "The Family Secret pt. 1", "2020-07-25", 0.79, 0.40, "Kenzie Reeves"),
	}
	rel := "All.Her.Luv_All.Her.Luv.The.Family.Secret.Pt2.Kenzie.Reeves.Helena.Locke"
	for _, c := range cands {
		a := Verify(cands, c.Scene.ID, c.Scene.Title, rel)
		b := VerifyWith(DefaultVerifyConfig, cands, c.Scene.ID, c.Scene.Title, rel)
		if a != b {
			t.Errorf("%s: Verify %+v != VerifyWith(default) %+v", c.Scene.ID, a, b)
		}
	}
}

// TopMargin refuses a confidence-driven verify when the field is flat. This is
// the measured failure: a performer-only query returns that performer's whole
// filmography, everything scores within a few hundredths, and the top slot is
// an artefact of the sort rather than a decision.
func TestTopMarginRefusesAFlatField(t *testing.T) {
	// No title overlap to speak of, so only the strong-match path can fire.
	cands := []Candidate{
		cand("wrong", "Slim Teen Chloe Cherry Sucks and Fucks a Massive Black Cock", "2016-09-06", 0.727, 0.03, "Chloe Cherry"),
		cand("right", "Chloe Cherry Seeks Out Massage Therapy For Her Ripe Behind", "2017-10-17", 0.714, 0.02, "Chloe Cherry"),
	}
	rel := "2021.01.16.Lethal.Hardcore.Chloe.Cherry.gets.turned.on.by.an.ass.massage.1080p"

	off := DefaultVerifyConfig
	if !VerifyWith(off, cands, "wrong", cands[0].Scene.Title, rel).Verified {
		t.Fatal("baseline: the flat field should verify today, that is the bug")
	}
	on := DefaultVerifyConfig
	on.TopMargin = 0.05
	if VerifyWith(on, cands, "wrong", cands[0].Scene.Title, rel).Verified {
		t.Error("with a 0.05 margin, a 0.013 lead must not verify")
	}
}

// StrongMatchNeedsDate stops the strong-match path trusting confidence when
// the release's date is confidently far from the scene's.
func TestStrongMatchNeedsDate(t *testing.T) {
	c := cand("wrong", "Slim Teen Chloe Cherry Sucks and Fucks", "2016-09-06", 0.75, 0.03, "Chloe Cherry")
	c.DateFarOff = true
	cands := []Candidate{c}
	rel := "2021.01.16.Lethal.Hardcore.Chloe.Cherry.gets.turned.on.1080p"

	if !VerifyWith(DefaultVerifyConfig, cands, "wrong", c.Scene.Title, rel).Verified {
		t.Fatal("baseline: strong-match ignores the date veto today, that is the bug")
	}
	on := DefaultVerifyConfig
	on.StrongMatchNeedsDate = true
	if VerifyWith(on, cands, "wrong", c.Scene.Title, rel).Verified {
		t.Error("with the veto on, a confidently-far date must block strong-match")
	}
}

// Both knobs default to off, so this commit ships no behaviour change.
func TestNewGatesDefaultOff(t *testing.T) {
	if DefaultVerifyConfig.TopMargin != 0 {
		t.Error("TopMargin must default to 0 until a sweep justifies otherwise")
	}
	if DefaultVerifyConfig.StrongMatchNeedsDate {
		t.Error("StrongMatchNeedsDate must default off until a sweep justifies otherwise")
	}
}

// ScoreReplay reports recall and precision together. A config that verifies
// nothing scores perfect precision, so neither number means anything alone.
func TestScoreReplayCountsBothSides(t *testing.T) {
	entries := []ReplayEntry{
		RecordCandidates("rel one", "a", []Candidate{
			cand("a", "A Very Distinctive Title Indeed", "2020-01-01", 0.9, 0.9, "Someone"),
		}),
		RecordCandidates("rel two", "missing", []Candidate{
			cand("x", "Something Else Entirely", "2020-01-01", 0.2, 0.05, "Nobody"),
		}),
	}
	s := ScoreReplay(DefaultVerifyConfig, entries)
	if s.Entries != 2 {
		t.Fatalf("entries = %d", s.Entries)
	}
	if s.RecallRate() < 0 || s.RecallRate() > 1 || s.CleanRate() > s.RecallRate() {
		t.Errorf("nonsensical rates: recall %.2f clean %.2f", s.RecallRate(), s.CleanRate())
	}
}

// The dump must survive a real file round trip, since that is how the sweep
// and any CI gate will consume it.
func TestSaveLoadReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.json")
	want := []ReplayEntry{RecordCandidates("a release", "id-1", []Candidate{
		cand("id-1", "Some Title", "2021-05-05", 0.66, 0.2, "A Performer"),
	})}
	if err := SaveReplay(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExpectedID != "id-1" ||
		len(got[0].Candidates) != 1 || got[0].Candidates[0].Confidence != 0.66 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if ScoreReplay(DefaultVerifyConfig, got) != ScoreReplay(DefaultVerifyConfig, want) {
		t.Error("a loaded dump must score identically to the one saved")
	}
}
