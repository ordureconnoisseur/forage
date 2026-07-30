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

	got := addMissingPerformers(scenes, byScene, localByStashDBID, map[string]string{})
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
	got := addMissingPerformers(scenes, byScene, map[string]string{}, map[string]string{})
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
	got := addMissingPerformers(scenes, byScene, map[string]string{}, map[string]string{})
	if len(got[0].Performers) != 0 {
		t.Errorf("performers = %d, want 0", len(got[0].Performers))
	}
}

// TestAddMissingPerformersRendersOwnedAsLocal is the reported bug: a card
// offering to ADD a performer the user already had.
//
// Cherry Kiss had just been created via the "+" (Stash id 1358). Two caches
// had not caught up: performer_cache is rebuilt on a multi-hour cadence, and
// the scene's own local_performer_ids on a 12h one. So the scene listed no
// local performer for her, and the merge treated her as un-owned.
//
// Ownership must be resolved against the performer index, not against the
// scene's stale id list — and an owned performer renders as the local
// performer they are rather than being skipped, because skipping left them
// with no pill at all.
func TestAddMissingPerformersRendersOwnedAsLocal(t *testing.T) {
	// The scene's own list is empty, exactly as it is before the 12h rebuild.
	scenes := []discoverScene{{StashDBID: "scene-cherry", Performers: nil}}
	byScene := map[string][]stashDBPerformer{
		"scene-cherry": {
			{ID: "sdb-cherry", Name: "Cherry Kiss"},
			{ID: "sdb-vince", Name: "Vince Karter"},
		},
	}
	// We DO own Cherry Kiss (just added, id 1358). Vince we do not.
	got := addMissingPerformers(scenes, byScene,
		map[string]string{"sdb-cherry": "1358"}, map[string]string{})

	if len(got[0].Performers) != 2 {
		t.Fatalf("performers = %d, want 2", len(got[0].Performers))
	}
	cherry, vince := got[0].Performers[0], got[0].Performers[1]
	if !cherry.Local {
		t.Error("Cherry Kiss marked un-owned; the card would offer to add someone already in the library")
	}
	if cherry.StashID != "1358" {
		t.Errorf("Cherry Kiss StashID = %q, want 1358 so the pill can navigate", cherry.StashID)
	}
	if vince.Local {
		t.Error("Vince Karter marked owned; his + would disappear")
	}
}

// TestAddMissingPerformersOwnedByNameOnly covers the other half: 308 of 1087
// local performers on the reference instance carry NO cross-id, so an
// id-only index calls every one of them un-owned.
func TestAddMissingPerformersOwnedByNameOnly(t *testing.T) {
	scenes := []discoverScene{{StashDBID: "scene-n", Performers: nil}}
	byScene := map[string][]stashDBPerformer{
		"scene-n": {{ID: "sdb-uncrossed", Name: "Uncrossed Ursula"}},
	}
	got := addMissingPerformers(scenes, byScene,
		map[string]string{}, // no cross-id for her
		map[string]string{"uncrossed ursula": "77"})

	if len(got[0].Performers) != 1 {
		t.Fatalf("performers = %d, want 1", len(got[0].Performers))
	}
	if p := got[0].Performers[0]; !p.Local || p.StashID != "77" {
		t.Errorf("got %+v, want local with StashID 77 — matched by name when no cross-id exists", p)
	}
}
