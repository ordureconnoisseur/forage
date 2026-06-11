package matcher

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// TestRankCandidates pins the full ordering contract: confidence
// descending, exact-tie broken by title overlap descending, remaining
// tie pinned by scene id ascending — and float noise below
// tiebreakerEpsilon treated as a tie.
func TestRankCandidates(t *testing.T) {
	c := func(id string, conf, overlap float64) Candidate {
		return Candidate{Scene: stashdb.Scene{ID: id}, Confidence: conf, TitleOverlap: overlap}
	}
	cands := []Candidate{
		c("d-noise", 0.70+tiebreakerEpsilon/10, 0.10), // float noise: ties with d/e at 0.70
		c("e-tie", 0.70, 0.10),
		c("a-top", 0.90, 0.05),
		c("b-overlap", 0.70, 0.30), // wins the 0.70 tie on overlap
		c("f-low", 0.20, 0.99),     // high overlap never beats higher confidence
	}
	rankCandidates(cands)
	want := []string{"a-top", "b-overlap", "d-noise", "e-tie", "f-low"}
	for i, w := range want {
		if cands[i].Scene.ID != w {
			t.Fatalf("rank %d = %s, want %s (full order: %v)", i+1, cands[i].Scene.ID, w, ids(cands))
		}
	}
}

func ids(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i := range cands {
		out[i] = cands[i].Scene.ID
	}
	return out
}
