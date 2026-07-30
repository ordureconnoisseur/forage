package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A watch added from a secondary box must REMEMBER which box. Without it the
// id gets resolved against StashDB, which does not error — it returns nothing,
// which is indistinguishable from "StashDB has not indexed this yet".
func TestWatchRemembersItsStashBox(t *testing.T) {
	s := newDeferTestServer(t)
	body := `{"stashdb_id":"fansdb-uuid","title":"A Scene",
	          "source":"https://fansdb.cc/graphql","image_url":"http://x/y.jpg",
	          "performers":["Someone"]}`
	rec := httptest.NewRecorder()
	s.postWatch(rec, httptest.NewRequest(http.MethodPost, "/watches",
		strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /watches = %d: %s", rec.Code, rec.Body.String())
	}

	w, err := s.watches.ByID(context.Background(), "fansdb-uuid")
	if err != nil || w == nil {
		t.Fatalf("watch not stored: %v", err)
	}
	if w.Source != "https://fansdb.cc/graphql" {
		t.Fatalf("source = %q, want the fansdb endpoint", w.Source)
	}

	// And it reaches the client, so the UI can tell the two apart.
	rec = httptest.NewRecorder()
	s.getWatches(rec, httptest.NewRequest(http.MethodGet, "/watches", nil))
	var out struct {
		Watches []struct {
			StashDBID string `json:"stashdb_id"`
			Source    string `json:"source"`
		} `json:"watches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, got := range out.Watches {
		if got.StashDBID == "fansdb-uuid" {
			if got.Source != "https://fansdb.cc/graphql" {
				t.Fatalf("listed source = %q", got.Source)
			}
			return
		}
	}
	t.Fatal("watch missing from the list")
}

// A plain StashDB watch stores no source. "" is the default for every row
// that existed before secondary boxes, so it must stay the quiet case.
func TestStashDBWatchHasEmptySource(t *testing.T) {
	s := newDeferTestServer(t)
	rec := httptest.NewRecorder()
	s.postWatch(rec, httptest.NewRequest(http.MethodPost, "/watches",
		strings.NewReader(`{"stashdb_id":"sdb-uuid","title":"A","image_url":"i","performers":["P"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	w, err := s.watches.ByID(context.Background(), "sdb-uuid")
	if err != nil || w == nil {
		t.Fatal(err)
	}
	if w.Source != "" {
		t.Fatalf("source = %q, want empty", w.Source)
	}
}

// stashDBFor must never hand back nil for an unknown source. Its callers are
// enrichment paths that treat a miss as "no extra detail"; a nil client turns
// that into a skipped branch, which is a different and worse failure.
func TestStashDBForFallsBackRatherThanNil(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	for _, src := range []string{
		"", "https://stashdb.org/graphql", "https://gone.example/graphql",
	} {
		// pool has no StashDB configured in this test server, so the
		// assertion is about the SELECTION, not the client itself.
		got := s.stashDBFor(ctx, src)
		if got != s.pool.StashDB() {
			t.Errorf("stashDBFor(%q) picked a different client than the primary fallback", src)
		}
	}
}
