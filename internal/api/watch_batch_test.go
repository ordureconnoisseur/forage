package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Subject pages group one-by-one adds with multi-selects: a single add
// carrying batch_id/batch_label lands in that group, and the bulk
// endpoint honours a caller-pinned batch_id so repeat selections merge.
// Without a batch_id both keep their original behaviour (ungrouped
// single / fresh generated batch).
func TestWatchAddsJoinCallerBatch(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()

	post := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if path == "/watches/batch" {
			s.postWatchBatch(rec, req)
		} else {
			s.postWatch(rec, req)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		return rec
	}

	// One-by-one adds from a performer page share its stable batch.
	post("/watches", `{"stashdb_id":"sc1","title":"One","batch_id":"performer:42","batch_label":"Perf X"}`)
	post("/watches", `{"stashdb_id":"sc2","title":"Two","batch_id":"performer:42","batch_label":"Perf X"}`)
	// A bare add stays ungrouped.
	post("/watches", `{"stashdb_id":"sc3","title":"Loose"}`)
	// A multi-select from the same page pins the same batch id.
	post("/watches/batch", `{"batch_id":"performer:42","batch_label":"Perf X","watches":[{"stashdb_id":"sc4","title":"Four"}]}`)

	list, err := s.watches.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, wt := range list {
		got[wt.StashDBID] = wt.BatchID
	}
	for _, id := range []string{"sc1", "sc2", "sc4"} {
		if got[id] != "performer:42" {
			t.Fatalf("%s batch = %q, want performer:42 (all: %v)", id, got[id], got)
		}
	}
	if got["sc3"] != "" {
		t.Fatalf("bare add batch = %q, want ungrouped", got["sc3"])
	}
	if wt := s.findWatch(ctx, "sc1"); wt == nil || wt.BatchLabel != "Perf X" {
		t.Fatalf("batch label not stored: %+v", wt)
	}
}
