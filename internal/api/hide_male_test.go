package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/config"
)

// The whole safety property of this feature in one test: only a KNOWN male is
// hidden. A performer with no recorded gender must survive, because "we never
// asked" and "they are male" are different facts and only one of them is a
// reason to remove someone from the screen.
func TestDropMalePerformersNeverHidesUnknownGender(t *testing.T) {
	scenes := []discoverScene{{
		StashDBID: "sc1",
		Performers: []discoverPerformer{
			{Name: "Owned Female", Local: true, Gender: "FEMALE"},
			{Name: "Owned Male", Local: true, Gender: "MALE"},
			{Name: "Owned Blank", Local: true, Gender: ""},
			{Name: "Unowned Male", StashDBID: "sdb-male"},
			{Name: "Unowned Female", StashDBID: "sdb-fem"},
			{Name: "Unowned Unfetched", StashDBID: "sdb-none"},
			{Name: "Unowned No Cross-id"},
		},
	}}
	// sdb-none is deliberately absent: the backfill has not reached them.
	byID := map[string]string{"sdb-male": "MALE", "sdb-fem": "FEMALE"}

	got := dropMalePerformers(scenes, byID)
	if len(got) != 1 {
		t.Fatalf("the scene itself must be kept, got %d scenes", len(got))
	}
	var names []string
	for _, p := range got[0].Performers {
		names = append(names, p.Name)
	}
	want := []string{"Owned Female", "Owned Blank", "Unowned Female",
		"Unowned Unfetched", "Unowned No Cross-id"}
	if len(names) != len(want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("kept %v, want %v", names, want)
		}
	}
}

// A scene whose entire cast is filtered out still appears. The setting hides
// performers; silently shrinking the feed as a side effect would be a
// different, unasked-for change.
func TestDropMalePerformersKeepsSceneWithNoPillsLeft(t *testing.T) {
	scenes := []discoverScene{{
		StashDBID:  "sc1",
		Performers: []discoverPerformer{{Name: "M", Local: true, Gender: "MALE"}},
	}}
	got := dropMalePerformers(scenes, nil)
	if len(got) != 1 {
		t.Fatalf("scene dropped: got %d", len(got))
	}
	if len(got[0].Performers) != 0 {
		t.Fatalf("performers = %v, want none", got[0].Performers)
	}
}

// The performer list honours the setting, and — the same rule as above — a
// blank gender is not a male. 166 of 1087 performers on the reference
// instance have no gender recorded, so treating blank as male would empty a
// sixth of the grid.
func TestGetPerformersHideMale(t *testing.T) {
	s := newDeferTestServer(t)
	seedCachedPerformer(t, s, "p1", "Alice", "FEMALE")
	seedCachedPerformer(t, s, "p2", "Bob", "MALE")
	seedCachedPerformer(t, s, "p3", "Blank", "")

	names := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		s.getPerformers(rec, httptest.NewRequest(http.MethodGet, "/performers", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /performers = %d: %s", rec.Code, rec.Body.String())
		}
		var out performersResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range out.Performers {
			got = append(got, p.Name)
		}
		return got
	}

	if got := names(); len(got) != 3 {
		t.Fatalf("off: got %v, want all three", got)
	}
	s.pool.Reload(config.Config{HideMalePerformers: true})
	got := names()
	if len(got) != 2 {
		t.Fatalf("on: got %v, want Alice and Blank", got)
	}
	for _, n := range got {
		if n == "Bob" {
			t.Fatalf("Bob is MALE and should be hidden: %v", got)
		}
	}
}

// The Discover response must not carry male pills once the setting is on,
// including the un-owned ones — which is the case the feature exists for and
// the one a naive gender check misses, since un-owned performers have no
// cached gender of their own.
func TestDiscoverHidesUnownedMalePills(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()

	// A recent, unowned scene with one local female and two StashDB-only
	// performers, one male and one whose gender was never fetched.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO recent_scene_cache
		  (stashdb_id, title, release_unix, owned, trending_rank, local_performer_ids, cached_at)
		VALUES ('sc1', 'Scene One', ?, 0, 0, '["p1"]', ?)`, nowUnix(), nowUnix()); err != nil {
		t.Fatal(err)
	}
	seedCachedPerformer(t, s, "p1", "Alice", "FEMALE")
	for _, p := range []struct{ id, name string }{
		{"sdb-m", "Male Costar"}, {"sdb-u", "Unfetched Costar"},
	} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO scene_performer (scene_id, performer_stashdb_id) VALUES ('sc1', ?)`,
			p.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO stashdb_scene (stashdb_id, title, performers)
		VALUES ('sc1', 'Scene One',
		  '[{"id":"sdb-m","name":"Male Costar"},{"id":"sdb-u","name":"Unfetched Costar"}]')`); err != nil {
		t.Fatal(err)
	}
	// Only the male has a recorded gender; the other is still unresolved.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO stashdb_performer_gender (stashdb_id, gender, fetched_at)
		VALUES ('sdb-m', 'MALE', 1)`); err != nil {
		t.Fatal(err)
	}

	pills := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		s.getDiscover(rec, httptest.NewRequest(http.MethodGet, "/discover", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /discover = %d: %s", rec.Code, rec.Body.String())
		}
		var out discoverResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, sc := range out.Scenes {
			for _, p := range sc.Performers {
				got = append(got, p.Name)
			}
		}
		return got
	}

	before := pills()
	if len(before) != 3 {
		t.Fatalf("off: pills = %v, want all three", before)
	}

	s.pool.Reload(config.Config{HideMalePerformers: true})
	after := pills()
	for _, n := range after {
		if n == "Male Costar" {
			t.Fatalf("on: male pill survived: %v", after)
		}
	}
	if len(after) != 2 {
		t.Fatalf("on: pills = %v, want Alice and the unfetched costar", after)
	}
}
