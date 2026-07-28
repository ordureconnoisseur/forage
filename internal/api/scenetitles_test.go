package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/db"
)

// TestCachedSceneTitles pins the lookup that keeps the Grabs list off the
// network. enrichSceneTitles used to resolve every multi-attempt scene with
// a serial StashDB FindScene inside the request, sharing the 4 req/s budget
// and memoised only in process, so each restart paid it again. The local
// scene cache already holds a title for nearly all of them.
func TestCachedSceneTitles(t *testing.T) {
	dbh, err := db.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	ctx := context.Background()
	for _, r := range []struct{ id, title string }{
		{"sc-1", "First Scene"},
		{"sc-2", "Second Scene"},
		{"sc-blank", ""}, // cached but title-less: must count as a miss
	} {
		if _, err := dbh.ExecContext(ctx,
			`INSERT INTO stashdb_scene (stashdb_id, title) VALUES (?, ?)`, r.id, r.title); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: dbh, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got := s.cachedSceneTitles(ctx, []string{"sc-1", "sc-2", "sc-blank", "sc-absent"})
	if len(got) != 2 {
		t.Fatalf("resolved %d titles (%v), want 2", len(got), got)
	}
	if got["sc-1"] != "First Scene" || got["sc-2"] != "Second Scene" {
		t.Errorf("wrong titles: %v", got)
	}
	// A row with an empty title must not shadow the network fallback: the
	// caller treats "" as a miss, so it must not appear in the map at all.
	if _, ok := got["sc-blank"]; ok {
		t.Error("empty cached title was returned as a hit; the network fallback would be skipped")
	}
	if _, ok := got["sc-absent"]; ok {
		t.Error("unknown id appeared in the result")
	}

	// Empty input must not build a malformed IN () clause.
	if n := len(s.cachedSceneTitles(ctx, nil)); n != 0 {
		t.Errorf("nil ids returned %d titles", n)
	}
}

// TestCachedSceneTitlesChunks pins that a page larger than the chunk size
// still resolves every id, rather than silently returning the first chunk.
func TestCachedSceneTitlesChunks(t *testing.T) {
	dbh, err := db.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	ctx := context.Background()
	var ids []string
	for i := 0; i < 1200; i++ {
		id := "sc-" + string(rune('a'+i%26)) + "-" + itoa(i)
		ids = append(ids, id)
		if _, err := dbh.ExecContext(ctx,
			`INSERT INTO stashdb_scene (stashdb_id, title) VALUES (?, ?)`, id, "T"+itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{db: dbh, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	got := s.cachedSceneTitles(ctx, ids)
	if len(got) != len(ids) {
		t.Fatalf("resolved %d of %d ids across chunks", len(got), len(ids))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
