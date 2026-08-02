package matcher

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

// Floors for the FULL pipeline: Match (retrieval + scoring against StashDB)
// followed by Verify.
//
// The replay gate in corpus_gate_test.go guards Verify and nothing else. It
// replays a frozen candidate set, so a regression in query construction,
// retrieval or the confidence weights is invisible to it by construction: the
// candidates were recorded before the change. The headline 0.953 is Match AND
// Verify together, and only the second half had a gate.
//
// These floors close that. They cannot run on every push (live StashDB, the
// daemon's performer/studio caches, ~26 minutes), so they are enforced by
// matcher-bench --gate on a schedule against the maintainer's instance. See
// docs/matcher-accuracy.md.
//
// Embedded rather than read from disk because the bench binary ships in the
// distroless image and runs as `docker exec forager /matcher-bench`, where
// there is no repo tree to read a file out of. Compiling the floors in also
// means the floors always describe the code being measured.
//
//go:embed pipeline_floors.json
var pipelineFloorsJSON []byte

// PipelineFloors is the recorded worst-acceptable full-pipeline result.
//
// Rates, not counts, for everything except MinEntries: the corpus is rebuilt
// from the daemon's confirmed grabs on every run and grows as the user grabs
// more, so an absolute cap on false verifies would tighten itself into a false
// alarm as the corpus doubles. MinEntries is the exception and is the point:
// a corpus that silently collapsed to 40 rows would otherwise sail past every
// rate floor.
type PipelineFloors struct {
	// Note, MeasuredAt and Commit are provenance, not thresholds. A floor
	// with no record of where it came from is a number nobody dares move.
	Note       string `json:"note"`
	MeasuredAt string `json:"measured_at"`
	Commit     string `json:"commit,omitempty"`

	MinEntries         int     `json:"min_entries"`
	MinRecall          float64 `json:"min_recall"`
	MinClean           float64 `json:"min_clean"`
	MaxFalseVerifyRate float64 `json:"max_false_verify_rate"`

	// Measured is what the run these floors were set from actually scored.
	// Kept so the slack between floor and measurement is visible in the file
	// rather than reconstructed from a commit message.
	Measured struct {
		Entries         int     `json:"entries"`
		Recall          float64 `json:"recall"`
		Clean           float64 `json:"clean"`
		FalseVerifyRate float64 `json:"false_verify_rate"`
	} `json:"measured"`
}

// DefaultPipelineFloors returns the floors compiled into this build.
func DefaultPipelineFloors() (PipelineFloors, error) {
	return parsePipelineFloors(pipelineFloorsJSON)
}

// LoadPipelineFloors reads floors from a file, for trying a proposed ratchet
// against a recorded run without rebuilding the bench binary.
func LoadPipelineFloors(path string) (PipelineFloors, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PipelineFloors{}, err
	}
	return parsePipelineFloors(b)
}

func parsePipelineFloors(b []byte) (PipelineFloors, error) {
	var f PipelineFloors
	if err := json.Unmarshal(b, &f); err != nil {
		return PipelineFloors{}, fmt.Errorf("parse pipeline floors: %w", err)
	}
	return f, nil
}

// FalseVerifyRate is false verifies per corpus entry. Above 1.0 is possible
// and means the average entry verified more than one wrong scene.
func (s ReplayScore) FalseVerifyRate() float64 {
	if s.Entries == 0 {
		return 0
	}
	return float64(s.FalseVerifies) / float64(s.Entries)
}

// Check returns one line per breached floor, empty when the run is acceptable.
//
// Returning every breach rather than the first is deliberate: recall and
// precision trade against each other, so a change that broke both should say
// so in one run instead of hiding the second failure behind the first.
func (f PipelineFloors) Check(s ReplayScore) []string {
	var breaches []string
	if s.Entries < f.MinEntries {
		breaches = append(breaches, fmt.Sprintf(
			"corpus holds %d entries, below the floor of %d: the corpus shrank, so every rate below is measured on the wrong thing",
			s.Entries, f.MinEntries))
	}
	if r := s.RecallRate(); r < f.MinRecall {
		breaches = append(breaches, fmt.Sprintf(
			"recall %.4f below floor %.4f: the pipeline no longer finds scenes it used to find",
			r, f.MinRecall))
	}
	if c := s.CleanRate(); c < f.MinClean {
		breaches = append(breaches, fmt.Sprintf(
			"clean rate %.4f below floor %.4f: fewer releases resolve to exactly one scene",
			c, f.MinClean))
	}
	if fv := s.FalseVerifyRate(); fv > f.MaxFalseVerifyRate {
		breaches = append(breaches, fmt.Sprintf(
			"false verify rate %.4f above cap %.4f: the pipeline accepts more wrong scenes",
			fv, f.MaxFalseVerifyRate))
	}
	return breaches
}
