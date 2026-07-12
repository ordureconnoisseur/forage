package prowlarr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// IndexerStatuses parses Prowlarr's failure-backoff list; entries with a
// cleared/absent disabledTill parse as zero times.
func TestIndexerStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/indexerstatus" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[
			{"indexerId": 3, "disabledTill": "2099-01-01T00:00:00Z"},
			{"indexerId": 7}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	out, err := c.IndexerStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d statuses, want 2", len(out))
	}
	if out[0].IndexerID != 3 || !out[0].DisabledTill.After(time.Now()) {
		t.Fatalf("entry 0 = %+v, want indexer 3 disabled into the future", out[0])
	}
	if out[1].IndexerID != 7 || !out[1].DisabledTill.IsZero() {
		t.Fatalf("entry 1 = %+v, want indexer 7 with zero disabledTill", out[1])
	}
}
