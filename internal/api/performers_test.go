package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/config"
)

// seedCachedPerformer inserts a minimal performer_cache row the way the
// cache refresh would, with just the columns the list endpoint reads.
func seedCachedPerformer(t *testing.T, s *Server, stashID, name, gender string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO performer_cache (stash_id, name, aliases, gender, refreshed_at)
		VALUES (?, ?, '[]', ?, 1)`, stashID, name, gender); err != nil {
		t.Fatal(err)
	}
}

// The Performers list ships what the plugin's in-memory content filter
// needs: each performer's cached gender, plus the deployment's full
// name→genders filter sets (the same config Discover uses). The server
// never narrows; a chip toggle filters client-side without a refetch.
// Unconfigured deployments stay dormant (no sets advertised).
func TestGetPerformersContentFilterData(t *testing.T) {
	s := newDeferTestServer(t)
	seedCachedPerformer(t, s, "p1", "Alice", "FEMALE")
	seedCachedPerformer(t, s, "p2", "Bella", "TRANSGENDER_FEMALE")

	get := func() performersResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		s.getPerformers(rec, httptest.NewRequest(http.MethodGet, "/performers", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /performers = %d: %s", rec.Code, rec.Body.String())
		}
		var out performersResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// Dormant: no config → everyone listed, no filter sets advertised.
	out := get()
	if len(out.Performers) != 2 || out.Filters != nil {
		t.Fatalf("dormant: got %d performers, filters %v", len(out.Performers), out.Filters)
	}
	genders := map[string]string{}
	for _, p := range out.Performers {
		genders[p.Name] = p.Gender
	}
	if genders["Alice"] != "FEMALE" || genders["Bella"] != "TRANSGENDER_FEMALE" {
		t.Fatalf("genders = %v", genders)
	}

	s.pool.Reload(config.Config{DiscoverFilters: "Trans=TRANSGENDER_FEMALE,TRANSGENDER_MALE"})

	out = get()
	if len(out.Performers) != 2 {
		t.Fatalf("configured filters must not narrow the list, got %d", len(out.Performers))
	}
	want := []string{"TRANSGENDER_FEMALE", "TRANSGENDER_MALE"}
	if got := out.Filters["Trans"]; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("filters = %v, want Trans=%v", out.Filters, want)
	}
}
