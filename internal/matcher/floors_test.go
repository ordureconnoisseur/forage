package matcher

import (
	"testing"
)

// The committed floors file has to parse and mean something. It is embedded,
// so a malformed edit compiles fine and only fails 26 minutes into a scheduled
// bench run, on a machine nobody is watching.
func TestPipelineFloorsAreSane(t *testing.T) {
	f, err := DefaultPipelineFloors()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		ok   bool
		why  string
	}{
		{"min entries set", f.MinEntries > 0,
			"a zero entry floor lets a corpus that collapsed to nothing pass every rate below it"},
		{"recall floor in range", f.MinRecall > 0 && f.MinRecall < 1,
			"a recall floor of 0 gates nothing and one of 1 fails every real run"},
		{"clean floor in range", f.MinClean > 0 && f.MinClean < 1,
			"same, for the precision-aware number that is the honest headline"},
		{"false verify cap set", f.MaxFalseVerifyRate > 0,
			"a cap of zero fails every run, so it would be turned off rather than fixed"},
		{"match error cap in range", f.MaxMatchErrorRate > 0 && f.MaxMatchErrorRate < 0.5,
			"zero fails a 26-minute run on one transient StashDB 5xx, and half the corpus erroring is not a run worth scoring at all"},
		{"clean not above recall", f.MinClean <= f.MinRecall,
			"clean is recall minus the entries that also verified something wrong, so a clean floor above the recall floor can never be met"},
		{"provenance recorded", f.MeasuredAt != "" && f.Note != "",
			"a floor with no record of where it came from is a number nobody dares move"},
	}
	for _, tt := range tests {
		if !tt.ok {
			t.Errorf("%s: %s", tt.name, tt.why)
		}
	}

	// Floors must sit under the measurement they were set from. A floor at or
	// above the measured value turns third-party noise (StashDB editing a
	// scene) into a red scheduled run, and a gate that cries wolf is a gate
	// that gets muted.
	if f.MinRecall >= f.Measured.Recall {
		t.Errorf("recall floor %.4f is not below the measured %.4f: no slack for StashDB drift",
			f.MinRecall, f.Measured.Recall)
	}
	if f.MinClean >= f.Measured.Clean {
		t.Errorf("clean floor %.4f is not below the measured %.4f", f.MinClean, f.Measured.Clean)
	}
	if f.MaxFalseVerifyRate <= f.Measured.FalseVerifyRate {
		t.Errorf("false verify cap %.4f is not above the measured %.4f",
			f.MaxFalseVerifyRate, f.Measured.FalseVerifyRate)
	}
	if f.MinEntries >= f.Measured.Entries {
		t.Errorf("entry floor %d is not below the measured %d: the corpus grows and shrinks between runs",
			f.MinEntries, f.Measured.Entries)
	}
}

// The `measured` block has to be a measurement of something this repo ships.
//
// TestPipelineFloorsAreSane above compares the hand-written floors against the
// hand-written `measured` block in the same hand-written file, which asserts
// the file back to itself: it would pass just as green on fabricated numbers.
// Nobody has run a live full-pipeline bench at this commit (see the file's
// note), so the only checkable claim the block makes is the one it actually
// makes: these are the committed fixture's candidate sets scored by the shipped
// verifier. This checks that claim, and it is what stops a future hand-edit of
// floors-plus-measured from passing while describing nothing.
//
// Four decimal places because that is the precision the file is written to.
//
// What this does NOT prove: that the numbers describe live retrieval today. The
// fixture's candidates were recorded at 627749f and 16bcab6 has since touched
// fold() in the retrieval path. Only a live run settles that, and this test
// cannot do one.
func TestPipelineFloorsMeasuredMatchTheCommittedFixture(t *testing.T) {
	f, err := DefaultPipelineFloors()
	if err != nil {
		t.Fatal(err)
	}
	entries := loadCorpusFixture(t)
	s := scoreOnce("default", DefaultVerifyConfig, entries)

	if s.Entries != f.Measured.Entries {
		t.Errorf("floors say the run scored %d entries, the committed fixture holds %d",
			f.Measured.Entries, s.Entries)
	}
	round4 := func(v float64) float64 { return float64(int64(v*1e4+0.5)) / 1e4 }
	for _, tt := range []struct {
		name            string
		got, claimed    float64
		whatItDescribes string
	}{
		{"recall", s.RecallRate(), f.Measured.Recall, "the expected scene verifies"},
		{"clean", s.CleanRate(), f.Measured.Clean, "the expected scene verifies and nothing else does"},
		{"false verify rate", s.FalseVerifyRate(), f.Measured.FalseVerifyRate, "wrong scenes accepted per entry"},
	} {
		if round4(tt.got) != tt.claimed {
			t.Errorf("pipeline_floors.json claims measured %s = %.4f (%s), but scoring the committed fixture under the shipped verifier gives %.4f. Either the verifier changed and the block is now stale, or the block was never measured. Write %.4f, or re-derive the floors from a real run.",
				tt.name, tt.claimed, tt.whatItDescribes, tt.got, round4(tt.got))
		}
	}
}

// The floors and the committed fixture must name the same recording.
//
// The floors were seeded from the fixture, so a floors file stamped with one
// commit and a fixture recorded at another is a provenance claim that is simply
// false, and provenance is the only thing standing behind numbers nobody has
// re-run live.
func TestPipelineFloorsNameTheCommittedRecording(t *testing.T) {
	f, err := DefaultPipelineFloors()
	if err != nil {
		t.Fatal(err)
	}
	meta := loadCorpusMeta(t)
	if f.Commit == "" {
		t.Fatal("pipeline_floors.json names no commit, so nothing says which tree it was seeded from")
	}
	if meta.Commit != f.Commit {
		t.Errorf("floors say they were seeded at %s but the committed fixture was recorded at %s: one of the two is describing a recording that is not in this tree",
			f.Commit, meta.Commit)
	}
}

// Check must be able to fail on each axis independently. A gate that only
// reports the first breach hides the second, and recall and precision trade
// against each other, so a bad change usually moves both.
func TestPipelineFloorsCheckCatchesEachAxis(t *testing.T) {
	floors := PipelineFloors{
		MinEntries: 100, MinRecall: 0.90, MinClean: 0.80, MaxFalseVerifyRate: 0.10,
		MaxMatchErrorRate: 0.02,
	}
	// 1000 entries scored, 950 recalled, 80 of those also verified a wrong
	// scene, 90 false verifies between them: recall 0.950, clean 0.870, false
	// rate 0.090, no match errors. Inside every floor, and internally
	// consistent, which the previous version of this comment was not: it
	// described 100 entries carrying 120 false verifies, a rate of 0.12 that
	// would breach this case's own 0.10 cap and fail the assertion it was
	// annotating, over a literal that said 90.
	pass := ReplayScore{Entries: 1000, Recall: 950, FalseVerifies: 90, EntriesWithFalse: 80}
	if got := floors.Check(pass); len(got) != 0 {
		t.Errorf("a run inside every floor was reported as a breach: %v", got)
	}

	tests := []struct {
		name  string
		score ReplayScore
		want  int
	}{
		{"corpus collapsed", ReplayScore{Entries: 50, Recall: 50}, 1},
		{"recall regressed", ReplayScore{Entries: 1000, Recall: 800, EntriesWithFalse: 0}, 1},
		{"precision regressed", ReplayScore{Entries: 1000, Recall: 950, FalseVerifies: 200, EntriesWithFalse: 100}, 1},
		// Recall dumped AND wrong scenes flooding in: both must be named, or
		// the second gets fixed only after the first is, one run per week.
		{"both directions", ReplayScore{Entries: 1000, Recall: 700, FalseVerifies: 300, EntriesWithFalse: 250}, 3},
		// StashDB unreachable for 5% of the corpus. The scored 950 are
		// pristine, so without the error axis this run passes every floor and
		// reports an outage as a clean bill of health.
		{"match errors", ReplayScore{Entries: 950, Recall: 950, MatchErrors: 50}, 1},
		// A handful of transient failures in a 26-minute run is not a
		// regression and must not read as one.
		{"a few match errors tolerated", ReplayScore{Entries: 990, Recall: 950, MatchErrors: 10}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := floors.Check(tt.score); len(got) != tt.want {
				t.Errorf("got %d breaches, want %d: %v", len(got), tt.want, got)
			}
		})
	}
}
