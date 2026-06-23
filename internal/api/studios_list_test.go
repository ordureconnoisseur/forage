package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/db"
)

// TestGetStudios exercises the /studios list query: ordering, search, and the
// favorite filter, straight against studio_cache (no StashDB needed — the
// aggregates are read as-cached).
func TestGetStudios(t *testing.T) {
	dbh, err := db.Open(t.TempDir() + "/forager.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	// Three studios: two cross-id'd (with aggregates), one synthetic (no
	// cross-id, zero aggregates) — all should list.
	seed := []struct {
		key, stashID, name, aliases string
		fav, scenes, total, owned   int
	}{
		{"sdb-blacked", "10", "Blacked", `["BLACKED"]`, 1, 120, 400, 120},
		{"sdb-vixen", "11", "Vixen", `["VIXEN"]`, 0, 80, 300, 80},
		{"stash:12", "12", "Local Only", `[]`, 0, 5, 0, 0},
	}
	for _, s := range seed {
		if _, err := dbh.Exec(`INSERT INTO studio_cache
			(stashdb_id, stash_id, name, aliases, favorite, scene_count, total_stashdb_scenes, owned_scenes_count, last_release_unix, refreshed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 1)`,
			s.key, s.stashID, s.name, s.aliases, s.fav, s.scenes, s.total, s.owned); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{db: dbh, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	call := func(query string) studiosResponse {
		req := httptest.NewRequest(http.MethodGet, "/studios?"+query, nil)
		rec := httptest.NewRecorder()
		s.getStudios(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /studios?%s = %d (%s)", query, rec.Code, rec.Body.String())
		}
		var out studiosResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Default sort = scene_count DESC: Blacked(120) > Vixen(80) > Local(5).
	def := call("")
	if len(def.Studios) != 3 {
		t.Fatalf("expected 3 studios, got %d", len(def.Studios))
	}
	if def.Studios[0].Name != "Blacked" || def.Studios[2].Name != "Local Only" {
		t.Errorf("scene_count order wrong: %s … %s", def.Studios[0].Name, def.Studios[2].Name)
	}
	// The synthetic studio carries its local fields but zero aggregates.
	last := def.Studios[2]
	if last.StashDBID != "stash:12" || last.TotalStashDBScenes != 0 || last.SceneCount != 5 {
		t.Errorf("synthetic studio wrong: %+v", last)
	}

	// missing_count sort: Vixen (300-80=220) > Blacked (400-120=280)? No —
	// Blacked 280 > Vixen 220, so Blacked first.
	miss := call("sort=missing_count")
	if miss.Studios[0].Name != "Blacked" {
		t.Errorf("missing_count order: first = %s, want Blacked", miss.Studios[0].Name)
	}

	// favorite_only returns just Blacked.
	favs := call("favorite_only=true")
	if len(favs.Studios) != 1 || favs.Studios[0].Name != "Blacked" {
		t.Errorf("favorite_only wrong: %+v", favs.Studios)
	}

	// search by alias (case-insensitive substring).
	q := call("q=vixen")
	if len(q.Studios) != 1 || q.Studios[0].Name != "Vixen" {
		t.Errorf("search wrong: %+v", q.Studios)
	}
}
