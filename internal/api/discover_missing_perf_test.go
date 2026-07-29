package api

import "testing"

// TestAddMissingPerformers pins the merge that makes an un-owned performer
// visible at all. Discover hydrates performers from performer_cache, which by
// definition only holds performers already in Stash, so a trending scene's
// unknown performer had no pill and therefore no route to being added. Every
// trending row on the reference instance had at least one LOCAL performer,
// which is why the cards looked populated while the interesting name was the
// one missing.
func TestAddMissingPerformers(t *testing.T) {
	scenes := []discoverScene{{
		StashDBID: "scene-1",
		Performers: []discoverPerformer{
			{StashID: "10", Name: "Local Lucy", Local: true},
		},
	}}
	byScene := map[string][]stashDBPerformer{
		"scene-1": {
			{ID: "sdb-lucy", Name: "Local Lucy"},    // same person, already a local pill
			{ID: "sdb-nina", Name: "Nina Newcomer"}, // genuinely missing
		},
	}
	// Lucy's cross-id is known locally, so she must not be duplicated.
	localByStashDBID := map[string]string{"sdb-lucy": "10"}

	got := addMissingPerformers(scenes, byScene, localByStashDBID)
	if len(got[0].Performers) != 2 {
		t.Fatalf("performers = %d, want 2 (one local + one added)", len(got[0].Performers))
	}
	added := got[0].Performers[1]
	if added.Name != "Nina Newcomer" || added.StashDBID != "sdb-nina" {
		t.Errorf("added = %+v, want Nina Newcomer / sdb-nina", added)
	}
	if added.Local {
		t.Error("added performer marked Local; the UI would offer no + button")
	}
	if !got[0].Performers[0].Local {
		t.Error("existing local performer lost its Local flag")
	}
}

// TestAddMissingPerformersDedupesByName covers a local performer with NO
// cross-id: the id map can't match them, so only the name can, and without
// that check they'd appear twice — once as their own pill and once as an
// "unowned" pill offering to create a duplicate.
func TestAddMissingPerformersDedupesByName(t *testing.T) {
	scenes := []discoverScene{{
		StashDBID:  "scene-2",
		Performers: []discoverPerformer{{StashID: "11", Name: "Uncrossed Ursula", Local: true}},
	}}
	byScene := map[string][]stashDBPerformer{
		"scene-2": {{ID: "sdb-ursula", Name: "uncrossed ursula"}}, // case differs
	}
	got := addMissingPerformers(scenes, byScene, map[string]string{})
	if len(got[0].Performers) != 1 {
		t.Fatalf("performers = %d, want 1 — a local performer without a cross-id "+
			"must still be matched by name, or the + would create a duplicate",
			len(got[0].Performers))
	}
}

// TestAddMissingPerformersSkipsJunk: a cached entry missing an id or a name
// cannot be created from, so it must not become a pill offering to try.
func TestAddMissingPerformersSkipsJunk(t *testing.T) {
	scenes := []discoverScene{{StashDBID: "scene-3"}}
	byScene := map[string][]stashDBPerformer{
		"scene-3": {{ID: "", Name: "No Id"}, {ID: "sdb-x", Name: ""}},
	}
	got := addMissingPerformers(scenes, byScene, map[string]string{})
	if len(got[0].Performers) != 0 {
		t.Errorf("performers = %d, want 0", len(got[0].Performers))
	}
}
