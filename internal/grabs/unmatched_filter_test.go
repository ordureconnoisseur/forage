package grabs

import (
	"context"
	"testing"
)

// seedConfirmedMix inserts one of each confirmed shape the split has to tell
// apart, plus a non-confirmed row so the partition can't accidentally sweep
// the whole table.
func seedConfirmedMix(t *testing.T, r *Repo) {
	t.Helper()
	ctx := context.Background()
	rows := []Grab{
		// linked single: a real match
		{ReleaseTitle: "linked", Status: "confirmed", Kind: "single",
			ActualStashDBID: "sdb-1", GrabbedAt: 1},
		// landed but never linked: adopted content StashDB doesn't have
		{ReleaseTitle: "unlinked-a", Status: "confirmed", Kind: "single",
			Reason: "in library (scanned)", GrabbedAt: 2},
		// same, from the predicted path that gave up
		{ReleaseTitle: "unlinked-b", Status: "confirmed", Kind: "single",
			PredictedStashDBID: "sdb-9", Reason: "in library; no StashDB match", GrabbedAt: 3},
		// a pack: no single cross-id by design, so NOT unmatched
		{ReleaseTitle: "pack", Status: "confirmed", Kind: "pack", GrabbedAt: 4},
		// not confirmed at all
		{ReleaseTitle: "downloading", Status: "downloading", Kind: "single", GrabbedAt: 5},
	}
	for _, g := range rows {
		if _, err := r.Insert(ctx, g); err != nil {
			t.Fatalf("insert %s: %v", g.ReleaseTitle, err)
		}
	}
}

// TestTotalsSplitsConfirmed: the two chips must partition the confirmed rows.
// If they overlapped or left a gap, the UI's "all" count (a sum of every
// value) would drift away from the real total.
func TestTotalsSplitsConfirmed(t *testing.T) {
	r := newTestRepo(t)
	seedConfirmedMix(t, r)

	totals, err := r.Totals(context.Background())
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if got := totals["unmatched"]; got != 2 {
		t.Errorf("unmatched = %d, want 2 (the two never-linked singles)", got)
	}
	if got := totals["confirmed"]; got != 2 {
		t.Errorf("confirmed = %d, want 2 (the linked single + the pack)", got)
	}
	sum := 0
	for _, n := range totals {
		sum += n
	}
	if sum != 5 {
		t.Errorf("values sum to %d, want 5 — the split must not double-count or drop rows", sum)
	}
}

// TestListUnmatchedFilter: the chip's count is worthless if clicking it shows
// a different set. List/CountFiltered must return exactly the rows Totals
// counted, for both halves.
func TestListUnmatchedFilter(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	seedConfirmedMix(t, r)

	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"unmatched", []string{"unlinked-b", "unlinked-a"}}, // grabbed_at DESC
		{"confirmed", []string{"pack", "linked"}},
	} {
		got, err := r.List(ctx, tc.status, "", 50, 0)
		if err != nil {
			t.Fatalf("List(%s): %v", tc.status, err)
		}
		titles := make([]string, 0, len(got))
		for _, g := range got {
			titles = append(titles, g.ReleaseTitle)
		}
		if len(titles) != len(tc.want) {
			t.Fatalf("List(%s) = %v, want %v", tc.status, titles, tc.want)
		}
		for i := range titles {
			if titles[i] != tc.want[i] {
				t.Fatalf("List(%s) = %v, want %v", tc.status, titles, tc.want)
			}
		}
		n, err := r.CountFiltered(ctx, tc.status, "")
		if err != nil {
			t.Fatalf("CountFiltered(%s): %v", tc.status, err)
		}
		if n != len(tc.want) {
			t.Errorf("CountFiltered(%s) = %d, want %d (must agree with List)", tc.status, n, len(tc.want))
		}
	}
}

// TestUnmatchedFilterComposesWithSearch: the search box narrows within the
// pseudo-status rather than replacing it.
func TestUnmatchedFilterComposesWithSearch(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	seedConfirmedMix(t, r)

	got, err := r.List(ctx, "unmatched", "unlinked-a", 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ReleaseTitle != "unlinked-a" {
		t.Fatalf("got %d rows, want just unlinked-a", len(got))
	}
}

// TestOtherStatusFiltersUnaffected: the switch that added the two special
// cases must leave every ordinary status alone.
func TestOtherStatusFiltersUnaffected(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	seedConfirmedMix(t, r)

	got, err := r.List(ctx, "downloading", "", 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Status != "downloading" {
		t.Fatalf("downloading filter returned %d rows, want 1", len(got))
	}
	all, err := r.List(ctx, "any", "", 50, 0)
	if err != nil {
		t.Fatalf("List(any): %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("List(any) returned %d rows, want all 5", len(all))
	}
}
