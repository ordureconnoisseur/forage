package cache

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

func sc(id, title, date, studioID, studioName string, perfs ...stashdb.ScenePerformer) stashdb.Scene {
	s := stashdb.Scene{ID: id, Title: title, Date: date, Updated: 100}
	if studioID != "" {
		s.Studio = &stashdb.SceneStudio{ID: studioID, Name: studioName}
	}
	s.Performers = perfs
	return s
}

func TestSceneCacheUpsertReadAggregate(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(filepath.Join(t.TempDir(), "forager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	// Subjects in the caches (the aggregate recompute updates these rows).
	if _, err := dbh.Exec(`INSERT INTO performer_cache (stash_id, stashdb_id, name, refreshed_at) VALUES ('1','perf-A','Slim Poke',0),('2','perf-B','Cyber Doll',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.Exec(`INSERT INTO studio_cache (stashdb_id, stash_id, name, refreshed_at) VALUES ('stud-X','10','Blacks on Blondes',0)`); err != nil {
		t.Fatal(err)
	}

	A := stashdb.ScenePerformer{ID: "perf-A", Name: "Slim Poke"}
	B := stashdb.ScenePerformer{ID: "perf-B", Name: "Cyber Doll"}
	scenes := []stashdb.Scene{
		sc("sc-1", "Wild Open House", "2026-06-19", "stud-X", "Blacks on Blondes", A, B),
		sc("sc-2", "Solo", "2026-06-10", "stud-X", "Blacks on Blondes", B),
		sc("sc-3", "Elsewhere", "2026-05-01", "", "", A), // no studio
	}
	for _, s := range scenes {
		if err := UpsertScene(ctx, dbh, s, 999); err != nil {
			t.Fatalf("upsert %s: %v", s.ID, err)
		}
	}
	// Idempotent re-upsert (and membership re-sync).
	if err := UpsertScene(ctx, dbh, scenes[0], 999); err != nil {
		t.Fatal(err)
	}

	// Read by performer: A → sc-1, sc-3; B → sc-1, sc-2.
	a, _ := ScenesForPerformer(ctx, dbh, "perf-A")
	if len(a) != 2 {
		t.Fatalf("perf-A scenes = %d, want 2", len(a))
	}
	// Newest release first.
	if a[0].ID != "sc-1" {
		t.Errorf("perf-A first scene = %s, want sc-1 (newest)", a[0].ID)
	}
	// Body reconstructed: studio + performers come back.
	if a[0].Studio == nil || a[0].Studio.Name != "Blacks on Blondes" || len(a[0].Performers) != 2 {
		t.Errorf("sc-1 body not reconstructed: %+v", a[0])
	}
	b, _ := ScenesForPerformer(ctx, dbh, "perf-B")
	if len(b) != 2 {
		t.Errorf("perf-B scenes = %d, want 2", len(b))
	}

	// Read by studio: stud-X → sc-1, sc-2 (sc-3 has no studio).
	st, _ := ScenesForStudio(ctx, dbh, "stud-X")
	if len(st) != 2 {
		t.Fatalf("stud-X scenes = %d, want 2", len(st))
	}

	// Recompute aggregates with sc-1 owned.
	if err := RecomputeAggregates(ctx, dbh, []string{"sc-1"}); err != nil {
		t.Fatal(err)
	}
	row := func(table, id string) (total, owned int, last int64) {
		dbh.QueryRow(`SELECT total_stashdb_scenes, owned_scenes_count, last_release_unix FROM `+table+` WHERE stashdb_id = ?`, id).Scan(&total, &owned, &last)
		return
	}
	tA, oA, _ := row("performer_cache", "perf-A")
	if tA != 2 || oA != 1 { // A in sc-1(owned) + sc-3 → total 2, owned 1
		t.Errorf("perf-A agg total=%d owned=%d, want 2/1", tA, oA)
	}
	tX, oX, _ := row("studio_cache", "stud-X")
	if tX != 2 || oX != 1 { // stud-X in sc-1(owned) + sc-2 → total 2, owned 1
		t.Errorf("stud-X agg total=%d owned=%d, want 2/1", tX, oX)
	}
}
