package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// The folder name comes from forage's own record and the cast from StashDB,
// so they name the same person without always spelling them identically. A
// false negative here re-files a scene into a second folder for someone who
// already has one.
func TestSameFolderName(t *testing.T) {
	same := [][2]string{
		{"Harley Love", "harley love"},
		{"Scarlett Rosewood", "Scarlett  Rosewood"},
		{"Alex Grey", "Alex-Grey"},
		{"J. Doe", "JDoe"},
	}
	for _, c := range same {
		if !sameFolderName(c[0], c[1]) {
			t.Errorf("sameFolderName(%q, %q) = false, want true", c[0], c[1])
		}
	}
	diff := [][2]string{
		{"Harley Love", "Harley Lovee"},
		{"Gigi Dior", "Kianna Dior"},
		{"", "Harley Love"},
		{"Harley Love", ""},
	}
	for _, c := range diff {
		if sameFolderName(c[0], c[1]) {
			t.Errorf("sameFolderName(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

// A mismatch answered one way must stay answerable the other way.
//
// /match rewrites actual_stashdb_id to whatever was chosen, so before this
// column existed, resolving a mismatch erased the fact that there had been
// one: reload the page and the panel offering the two scenes was simply gone,
// with no way back. phash_stashdb_id records what Stash's fingerprint said and
// nothing overwrites it.
func TestMatchPreservesThePhashVerdict(t *testing.T) {
	s := newDeferTestServer(t)
	const predicted = "019faec1-4bb6-78f1-a81c-80c779763120"
	const phash = "019baa2e-809d-7049-bc6a-ad9f7584f8ca"

	id, err := s.grabs.Insert(t.Context(), grabs.Grab{
		ReleaseTitle:       "Some Release",
		PredictedStashDBID: predicted,
		ActualStashDBID:    phash,
		PhashStashDBID:     phash,
		Status:             "mismatched",
		GrabbedAt:          1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Answer it the other way, the way /match does.
	if err := s.applyGrabUpdate(t.Context(), id, func(fresh *grabs.Grab) {
		fresh.ActualStashDBID = predicted
		fresh.Status = "confirmed"
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.grabs.Get(t.Context(), id)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.ActualStashDBID != predicted {
		t.Errorf("actual = %q, want the chosen scene %q", got.ActualStashDBID, predicted)
	}
	if got.PhashStashDBID != phash {
		t.Errorf("phash verdict = %q, want it preserved as %q; without it the "+
			"panel cannot offer the other scene back", got.PhashStashDBID, phash)
	}
}

// Backfill: rows written before the column existed carry the disagreement only
// in actual_stashdb_id, and /match is about to overwrite it.
func TestMatchBackfillsThePhashVerdict(t *testing.T) {
	s := newDeferTestServer(t)
	const predicted = "019faec1-4bb6-78f1-a81c-80c779763120"
	const phash = "019baa2e-809d-7049-bc6a-ad9f7584f8ca"

	id, err := s.grabs.Insert(t.Context(), grabs.Grab{
		ReleaseTitle:       "Older Release",
		PredictedStashDBID: predicted,
		ActualStashDBID:    phash, // no PhashStashDBID: pre-migration row
		Status:             "mismatched",
		GrabbedAt:          1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyGrabUpdate(t.Context(), id, func(fresh *grabs.Grab) {
		if fresh.PhashStashDBID == "" && fresh.ActualStashDBID != "" &&
			fresh.ActualStashDBID != fresh.PredictedStashDBID {
			fresh.PhashStashDBID = fresh.ActualStashDBID
		}
		fresh.ActualStashDBID = predicted
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.grabs.Get(t.Context(), id)
	if got.PhashStashDBID != phash {
		t.Errorf("phash verdict = %q, want %q backfilled from the old row",
			got.PhashStashDBID, phash)
	}
}
