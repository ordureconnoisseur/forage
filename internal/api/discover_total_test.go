package api

import "testing"

// The denominator has to mean "this window with your filters off", not "what
// the query returned". Measured before the already-grabbed drop it was wrong
// by exactly that drop, and an unfiltered page read "380 of 389": nine hidden
// scenes invented out of a step the user has no control over.
func TestKeepFavouritesIsSeparableFromTheCount(t *testing.T) {
	fav := discoverPerformer{Name: "Kept", Favorite: true}
	plain := discoverPerformer{Name: "Other"}
	in := []discoverScene{
		{StashDBID: "a", Performers: []discoverPerformer{fav}},
		{StashDBID: "b", Performers: []discoverPerformer{plain}},
		{StashDBID: "c", Performers: []discoverPerformer{plain, fav}},
		{StashDBID: "d"},
	}
	got := keepFavourites(in)
	if len(got) != 2 {
		t.Fatalf("kept %d, want 2 (a and c)", len(got))
	}
	if got[0].StashDBID != "a" || got[1].StashDBID != "c" {
		t.Errorf("kept %v, want a and c", []string{got[0].StashDBID, got[1].StashDBID})
	}
	// The whole point: the input is still available to be counted.
	if len(in) != 4 {
		t.Errorf("input was mutated: len %d, want 4", len(in))
	}
}
