package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/matcher"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The gate is the whole point of the scheduled run: a bench that prints a bad
// number and exits 0 is a bench nobody notices. This pins that a breach
// returns false (main turns that into exit 1) and that the failing metric is
// named in the output, because a scheduled job's only reader is a log.
func TestCheckFloorsFailsOnBreachAndSaysWhy(t *testing.T) {
	floorsPath := filepath.Join(t.TempDir(), "floors.json")
	writeFloors(t, floorsPath, `{
		"note": "test", "measured_at": "2026-07-31",
		"min_entries": 100, "min_recall": 0.90, "min_clean": 0.80,
		"max_false_verify_rate": 0.10
	}`)

	tests := []struct {
		name     string
		result   verifyResult
		wantPass bool
		wantSaid string
	}{
		{
			name:     "inside every floor",
			result:   verifyResult{Total: 1000, Recall: 950, FalseVerifies: 90, EntriesWithFalse: 100},
			wantPass: true,
			wantSaid: "gate: PASS",
		},
		{
			// Retrieval collapsing is exactly what the frozen replay gate
			// cannot see, so it is the case this whole apparatus exists for.
			name:     "recall collapsed",
			result:   verifyResult{Total: 1000, Recall: 500},
			wantPass: false,
			wantSaid: "no longer finds scenes",
		},
		{
			// Verifying everything scores perfect recall. Without the
			// precision arm the gate would call that an improvement.
			name:     "wrong scenes flooding in",
			result:   verifyResult{Total: 1000, Recall: 1000, FalseVerifies: 900, EntriesWithFalse: 400},
			wantPass: false,
			wantSaid: "accepts more wrong scenes",
		},
		{
			// A corpus that failed to build leaves a handful of rows whose
			// rates can look pristine.
			name:     "corpus collapsed",
			result:   verifyResult{Total: 40, Recall: 40},
			wantPass: false,
			wantSaid: "corpus scored",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := tt.result
			got := checkFloors(&buf, quietLogger(), floorsPath, &r)
			if got != tt.wantPass {
				t.Errorf("checkFloors = %v, want %v\n%s", got, tt.wantPass, buf.String())
			}
			if !strings.Contains(buf.String(), tt.wantSaid) {
				t.Errorf("output does not mention %q:\n%s", tt.wantSaid, buf.String())
			}
		})
	}
}

// An unreadable floors file must fail the run, not wave it through. The
// alternative is a scheduled gate that silently stops gating the day someone
// mistypes a path.
func TestCheckFloorsFailsWhenFloorsUnreadable(t *testing.T) {
	var buf bytes.Buffer
	r := verifyResult{Total: 1000, Recall: 1000}
	if checkFloors(&buf, quietLogger(), filepath.Join(t.TempDir(), "nope.json"), &r) {
		t.Error("a missing floors file passed the gate")
	}
}

// A refresh has to write both halves: the recording and the sidecar the corpus
// gate asserts against. Writing only the first leaves the gate pinned to a run
// that no longer exists.
func TestWriteDumpWritesFixtureAndSidecar(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "corpus-replay.json.gz")
	vr := &verifyResult{
		Total: 3, Recall: 2, FalseVerifies: 1, EntriesWithFalse: 1,
		// Deliberately out of order: runVerify appends in worker-completion
		// order, so an unsorted dump is byte-different every run and the
		// refreshed fixture is unreviewable.
		Replay: []matcher.ReplayEntry{
			{Release: "c", ExpectedID: "3"},
			{Release: "a", ExpectedID: "1"},
			{Release: "b", ExpectedID: "2"},
		},
	}
	if err := writeDump(quietLogger(), dump, "deadbee", "3 test entries", vr); err != nil {
		t.Fatal(err)
	}

	entries, err := matcher.LoadReplay(dump)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"a", "b", "c"} {
		if entries[i].Release != want {
			t.Errorf("entry %d is %q, want %q: the dump was not sorted", i, entries[i].Release, want)
		}
	}

	meta, err := matcher.LoadReplayMeta(matcher.ReplayMetaPath(dump))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Entries != 3 || meta.Recall != 2 || meta.FalseVerifies != 1 || meta.EntriesWithFalse != 1 {
		t.Errorf("sidecar does not describe the run: %+v", meta)
	}
	if meta.Commit != "deadbee" || meta.Corpus != "3 test entries" {
		t.Errorf("sidecar lost its provenance: %+v", meta)
	}
	if meta.Config != matcher.DefaultVerifyConfig {
		t.Error("sidecar must record the verifier that actually ran")
	}
}

// fakeMatcher drives runVerify without a live StashDB or a populated SQLite.
// Nothing could before the releaseMatcher interface existed, which is why the
// accounting below went unnoticed.
type fakeMatcher struct {
	cands map[string][]matcher.Candidate
	errs  map[string]error
}

func (f fakeMatcher) Match(_ context.Context, release string) ([]matcher.Candidate, error) {
	if err, ok := f.errs[release]; ok {
		return nil, err
	}
	return f.cands[release], nil
}

func sceneCandidate(id, title string, conf float64) matcher.Candidate {
	return matcher.Candidate{
		Scene:        stashdb.Scene{ID: id, Title: title},
		Confidence:   conf,
		TitleOverlap: 1,
	}
}

// A Match error is an entry the pipeline was never asked about. It must leave
// the run's denominator, not sit in it as a recall miss.
//
// Both halves of this mattered. In the denominator, a StashDB outage or the
// run's 60-minute deadline reads as "the pipeline no longer finds scenes it
// used to find" and trips a gate whose recall slack is ~36 entries of 1,570,
// with no number printed anywhere to tell an outage from a regression. And an
// errored entry counted in Total but absent from the --dump slice is what makes
// a refreshed fixture disagree with its own sidecar.
func TestRunVerifyExcludesMatchErrorsFromTheScore(t *testing.T) {
	entries := []stash.LabeledScene{
		{ID: "1", Basename: "Studio - The Real Title - 2021-01-01", StashDBID: "scene-1", Source: "grabs-search"},
		{ID: "2", Basename: "Studio - Unreachable - 2021-02-02", StashDBID: "scene-2", Source: "grabs-search"},
		{ID: "3", Basename: "Studio - Also Real - 2021-03-03", StashDBID: "scene-3", Source: "grabs-search"},
	}
	fm := fakeMatcher{
		cands: map[string][]matcher.Candidate{
			entries[0].Basename: {sceneCandidate("scene-1", "The Real Title", 0.95)},
			// Retrieved nothing: a genuine recall miss, which must still count.
			entries[2].Basename: {},
		},
		errs: map[string]error{
			entries[1].Basename: errors.New("stashdb: 503"),
		},
	}

	// concurrency 1 so the assertions below are about accounting and not about
	// which worker finished first.
	r := runVerify(context.Background(), quietLogger(), fm, entries, 1, true)

	if r.Total != 2 {
		t.Errorf("Total = %d, want 2: the errored entry must not be scored", r.Total)
	}
	if r.MatchErrors != 1 {
		t.Errorf("MatchErrors = %d, want 1", r.MatchErrors)
	}
	if len(r.Replay) != r.Total {
		t.Errorf("dump holds %d entries but %d were scored: this is the skew that commits a fixture disagreeing with its sidecar", len(r.Replay), r.Total)
	}
	for _, f := range r.Failures {
		if f.Release == entries[1].Basename {
			t.Errorf("the errored entry was recorded as a %q failure: an outage is not a retrieval regression", f.Kind)
		}
	}
	// One of the two scored entries verified, so recall is 1/2. Scoring the
	// error as a miss would give 1/3 and read as a 17-point recall drop that
	// no code change caused.
	if got := r.score().RecallRate(); got != 0.5 {
		t.Errorf("recall = %.4f, want 0.5000: the error is in the denominator", got)
	}
	if got := r.score().MatchErrorRate(); got != 1.0/3.0 {
		t.Errorf("match error rate = %.4f, want %.4f (1 of 3 attempted)", got, 1.0/3.0)
	}
}

// The same run, taken all the way through the refresh path: a --dump run with a
// Match error in it must still write a fixture and a sidecar that agree, since
// TestCorpusFixtureMatchesTheLiveRun compares exactly those two numbers on the
// next push and a disagreement can only be cleared by another 26-minute run.
func TestRefreshWithAMatchErrorStillWritesAConsistentPair(t *testing.T) {
	entries := []stash.LabeledScene{
		{ID: "1", Basename: "Studio - The Real Title - 2021-01-01", StashDBID: "scene-1"},
		{ID: "2", Basename: "Studio - Unreachable - 2021-02-02", StashDBID: "scene-2"},
	}
	fm := fakeMatcher{
		cands: map[string][]matcher.Candidate{
			entries[0].Basename: {sceneCandidate("scene-1", "The Real Title", 0.95)},
		},
		errs: map[string]error{entries[1].Basename: errors.New("stashdb: 503")},
	}
	r := runVerify(context.Background(), quietLogger(), fm, entries, 1, true)

	dump := filepath.Join(t.TempDir(), "corpus-replay.json.gz")
	if err := writeDump(quietLogger(), dump, "deadbee", "2 test entries", r); err != nil {
		t.Fatalf("a run with one transient Match error could not be recorded: %v", err)
	}
	fixture, err := matcher.LoadReplay(dump)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := matcher.LoadReplayMeta(matcher.ReplayMetaPath(dump))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Entries != len(fixture) {
		t.Errorf("sidecar claims %d entries, fixture holds %d: push CI would go red on this pair", meta.Entries, len(fixture))
	}
	if meta.MatchErrors != 1 {
		t.Errorf("sidecar records %d match errors, want 1: a fixture short of the corpus must say why", meta.MatchErrors)
	}
}

// And if that invariant is ever broken again upstream of here, the refresh must
// fail loudly at the point of writing rather than commit the broken pair.
func TestWriteDumpRefusesAFixtureThatDisagreesWithItsSidecar(t *testing.T) {
	vr := &verifyResult{
		Total:  3, // as if Total had been set to the corpus size up front
		Recall: 2,
		Replay: []matcher.ReplayEntry{{Release: "a"}, {Release: "b"}},
	}
	dump := filepath.Join(t.TempDir(), "corpus-replay.json.gz")
	err := writeDump(quietLogger(), dump, "", "", vr)
	if err == nil {
		t.Fatal("a fixture with 2 entries and a sidecar claiming 3 was written")
	}
	if _, statErr := os.Stat(dump); statErr == nil {
		t.Error("the fixture was written anyway; the refresh must leave the old one in place")
	}
}

// The gate has to name a Match outage as a Match outage. The failure mode it
// replaces is the expensive one: a scheduled run reporting an infrastructure
// problem as an accuracy regression, with nothing in its output to tell them
// apart.
func TestCheckFloorsReportsMatchErrorsAsThemselves(t *testing.T) {
	floorsPath := filepath.Join(t.TempDir(), "floors.json")
	writeFloors(t, floorsPath, `{
		"note": "test", "measured_at": "2026-07-31",
		"min_entries": 100, "min_recall": 0.90, "min_clean": 0.80,
		"max_false_verify_rate": 0.10, "max_match_error_rate": 0.02
	}`)

	// 900 of 1000 entries scored, all of them perfectly. The tenth of the
	// corpus that errored is the only thing wrong with this run.
	var buf bytes.Buffer
	r := verifyResult{Total: 900, Recall: 900, MatchErrors: 100}
	if checkFloors(&buf, quietLogger(), floorsPath, &r) {
		t.Error("a run that could not reach StashDB for a tenth of the corpus passed the gate")
	}
	out := buf.String()
	if !strings.Contains(out, "Match failed for 100 of 1000") {
		t.Errorf("the gate does not name the match failures:\n%s", out)
	}
	if strings.Contains(out, "no longer finds scenes") {
		t.Errorf("the gate blamed retrieval for a Match outage:\n%s", out)
	}
	// The count belongs in the table too, so a PASSING run with a few errors
	// still shows how many, rather than burying them in the run's stderr.
	if !strings.Contains(out, "match errors") {
		t.Errorf("the gate's table has no match-error row:\n%s", out)
	}
}

func writeFloors(t *testing.T, path, body string) {
	t.Helper()
	var probe map[string]any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("test floors are not valid JSON: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
