package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/matcher"
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
			wantSaid: "corpus holds",
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
