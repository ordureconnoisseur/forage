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

// The Performers list honours the same deployment-configured content
// filters as Discover: ?flt=<name> narrows to the named gender set, an
// unknown/absent name returns everyone, and the configured filter names
// ride along in the response for the UI's chips. Unconfigured
// deployments stay dormant (no names, no narrowing).
func TestGetPerformersContentFilter(t *testing.T) {
	s := newDeferTestServer(t)
	seedCachedPerformer(t, s, "p1", "Alice", "FEMALE")
	seedCachedPerformer(t, s, "p2", "Bella", "TRANSGENDER_FEMALE")
	seedCachedPerformer(t, s, "p3", "Carl", "MALE")

	get := func(url string) performersResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		s.getPerformers(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", url, rec.Code, rec.Body.String())
		}
		var out performersResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// Dormant: no config → all performers, no filter names advertised.
	out := get("/performers?flt=Trans")
	if len(out.Performers) != 3 || out.Filters != nil {
		t.Fatalf("dormant: got %d performers, filters %v", len(out.Performers), out.Filters)
	}

	s.pool.Reload(config.Config{DiscoverFilters: "Trans=TRANSGENDER_FEMALE,TRANSGENDER_MALE"})

	out = get("/performers?flt=Trans")
	if len(out.Performers) != 1 || out.Performers[0].Name != "Bella" {
		t.Fatalf("flt=Trans kept %+v", out.Performers)
	}
	if len(out.Filters) != 1 || out.Filters[0] != "Trans" {
		t.Fatalf("filters = %v, want [Trans]", out.Filters)
	}

	// No flt param, and a stale/unknown name, both mean no narrowing.
	if out = get("/performers"); len(out.Performers) != 3 {
		t.Fatalf("unfiltered kept %d", len(out.Performers))
	}
	if out = get("/performers?flt=Nope"); len(out.Performers) != 3 {
		t.Fatalf("unknown filter name kept %d, want all 3", len(out.Performers))
	}
}
