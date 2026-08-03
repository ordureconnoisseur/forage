package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWatchCandidatesCarryNoExplanation guards the one invariant that keeps the
// new payload out of the database.
//
// sceneRelease is marshalled straight into the watches table via
// watchCandidatesJSON, up to watchCandidateCap rows per available watch. The
// verdict explanation averages 1.7 KB, so 25 of them is ~43 KB per row on a
// table that has already had to shed 13.9 MB of candidate payload once. Before
// this test the only thing preventing that was an `explain=false` argument at
// two call sites, and nothing asserted it.
func TestWatchCandidatesCarryNoExplanation(t *testing.T) {
	// Deliberately set: the point is that the storage path strips it even when
	// a caller gets the flag wrong, which is the failure mode a bool argument
	// invites.
	cands := []sceneRelease{
		{
			Title:       "Some.Release.1080p",
			DownloadURL: "http://tracker/1",
			Verified:    true,
			Explain: &matchExplain{
				Verified: true,
				Path:     "strong-match",
				Gates:    []explainGate{{Name: "strong-match", Passed: true}},
			},
		},
		{
			Title:       "Another.Release.1080p",
			DownloadURL: "http://tracker/2",
			Verified:    true,
		},
	}

	raw := watchCandidatesJSON(cands)
	if strings.Contains(string(raw), "explain") {
		t.Fatalf("the stored watch row carries the verdict explanation:\n%s", raw)
	}

	// And it still stores the releases themselves, or the assertion above
	// would pass on an empty list.
	var stored []map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d candidates, expected 2", len(stored))
	}

	// The caller's own slice must be untouched: the interactive list hands the
	// same rows to the HTTP response.
	if cands[0].Explain == nil {
		t.Error("watchCandidatesJSON cleared the caller's explanation, which the release list still needs")
	}
}
