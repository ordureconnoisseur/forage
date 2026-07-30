package cache

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/db"
)

func genderTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbh, err := db.Open(t.TempDir() + "/g.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	return dbh
}

// The backlog query has to ask about exactly the right people: performers on
// cached scenes, not already answered, and NOT ones we own. Owned performers
// have a gender from Stash already, so including them would spend StashDB
// budget re-deriving a fact forage has locally.
func TestPerformersMissingGenderSelectsOnlyUnknownUnowned(t *testing.T) {
	dbh := genderTestDB(t)
	ctx := context.Background()

	if _, err := dbh.ExecContext(ctx, `
		INSERT INTO recent_scene_cache (stashdb_id, release_unix, cached_at, local_performer_ids)
		VALUES ('sc1', 1, 1, '[]')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"want-me", "already-known", "i-own-them"} {
		if _, err := dbh.ExecContext(ctx,
			`INSERT INTO scene_performer (scene_id, performer_stashdb_id) VALUES ('sc1', ?)`,
			id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO stashdb_performer_gender (stashdb_id, gender, fetched_at)
		 VALUES ('already-known', 'FEMALE', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO performer_cache (stash_id, stashdb_id, name, aliases, gender, refreshed_at)
		 VALUES ('1', 'i-own-them', 'Owned', '[]', 'FEMALE', 1)`); err != nil {
		t.Fatal(err)
	}

	got, err := PerformersMissingGender(ctx, dbh, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "want-me" {
		t.Fatalf("got %v, want [want-me]", got)
	}
}

// An empty gender must be STORED, not skipped. StashDB genuinely records no
// gender for some performers; if "no answer" were left unrecorded the backlog
// query would return them forever and the pass would never converge.
func TestStoreGendersRecordsEmptyAnswers(t *testing.T) {
	dbh := genderTestDB(t)
	ctx := context.Background()
	if _, err := dbh.ExecContext(ctx, `
		INSERT INTO recent_scene_cache (stashdb_id, release_unix, cached_at, local_performer_ids)
		VALUES ('sc1', 1, 1, '[]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO scene_performer (scene_id, performer_stashdb_id) VALUES ('sc1', 'no-gender')`); err != nil {
		t.Fatal(err)
	}
	if err := StoreGenders(ctx, dbh, map[string]string{"no-gender": ""}, 1); err != nil {
		t.Fatal(err)
	}
	left, err := PerformersMissingGender(ctx, dbh, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("still asking for %v; the pass would never settle", left)
	}
	got, err := GendersByStashDBID(ctx, dbh, []string{"no-gender"})
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := got["no-gender"]; !ok || g != "" {
		t.Fatalf("recorded gender = %q (present=%v), want an empty recorded answer", g, ok)
	}
}

// Genders are normalised and re-fetching updates rather than duplicating.
func TestStoreGendersUpsertsAndUppercases(t *testing.T) {
	dbh := genderTestDB(t)
	ctx := context.Background()
	if err := StoreGenders(ctx, dbh, map[string]string{"p": " male "}, 1); err != nil {
		t.Fatal(err)
	}
	got, _ := GendersByStashDBID(ctx, dbh, []string{"p"})
	if got["p"] != "MALE" {
		t.Fatalf("gender = %q, want MALE", got["p"])
	}
	if err := StoreGenders(ctx, dbh, map[string]string{"p": "FEMALE"}, 2); err != nil {
		t.Fatal(err)
	}
	got, _ = GendersByStashDBID(ctx, dbh, []string{"p"})
	if got["p"] != "FEMALE" {
		t.Fatalf("gender = %q after re-fetch, want FEMALE", got["p"])
	}
}

// Lookups are chunked; a list past SQLite's default 999-variable ceiling must
// not fail. Discover can reference thousands of performers in one response.
func TestGendersByStashDBIDChunksLargeIDLists(t *testing.T) {
	dbh := genderTestDB(t)
	ctx := context.Background()
	ids := make([]string, 0, 2500)
	want := map[string]string{}
	for i := 0; i < 2500; i++ {
		id := "p" + strconv.Itoa(i)
		ids = append(ids, id)
		if i%2 == 0 {
			want[id] = "MALE"
		}
	}
	if err := StoreGenders(ctx, dbh, want, 1); err != nil {
		t.Fatal(err)
	}
	got, err := GendersByStashDBID(ctx, dbh, ids)
	if err != nil {
		t.Fatalf("chunking failed: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d genders, want %d", len(got), len(want))
	}
}

// The backlog is scoped to the Discover window, not the whole persistent scene
// cache. Those are 1,014 performers versus 50,420 on the reference instance,
// and only the first set is ever rendered as a pill — an unscoped query spends
// hours of StashDB budget on performers nobody sees.
func TestPerformersMissingGenderIgnoresScenesOutsideDiscover(t *testing.T) {
	dbh := genderTestDB(t)
	ctx := context.Background()

	// Two cached scenes; only one is in the Discover window.
	if _, err := dbh.ExecContext(ctx, `
		INSERT INTO recent_scene_cache (stashdb_id, release_unix, cached_at, local_performer_ids)
		VALUES ('in-window', 1, 1, '[]')`); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ scene, perf string }{
		{"in-window", "shown"},
		{"archived", "never-rendered"},
	} {
		if _, err := dbh.ExecContext(ctx,
			`INSERT INTO scene_performer (scene_id, performer_stashdb_id) VALUES (?, ?)`,
			r.scene, r.perf); err != nil {
			t.Fatal(err)
		}
	}

	got, err := PerformersMissingGender(ctx, dbh, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "shown" {
		t.Fatalf("got %v, want only [shown]", got)
	}
}
