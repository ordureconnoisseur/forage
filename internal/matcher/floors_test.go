package matcher

import "testing"

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

// Check must be able to fail on each axis independently. A gate that only
// reports the first breach hides the second, and recall and precision trade
// against each other, so a bad change usually moves both.
func TestPipelineFloorsCheckCatchesEachAxis(t *testing.T) {
	floors := PipelineFloors{
		MinEntries: 100, MinRecall: 0.90, MinClean: 0.80, MaxFalseVerifyRate: 0.10,
	}
	// 1000 entries, 950 recalled, 100 entries carrying a false verify, 120
	// false verifies total: recall 0.95, clean 0.85, false rate 0.12.
	pass := ReplayScore{Entries: 1000, Recall: 950, FalseVerifies: 90, EntriesWithFalse: 100}
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := floors.Check(tt.score); len(got) != tt.want {
				t.Errorf("got %d breaches, want %d: %v", len(got), tt.want, got)
			}
		})
	}
}
