package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/watches"

	_ "modernc.org/sqlite"
)

// routerForTest builds a Server complete enough to answer the two public
// routes this file exercises, and returns the REAL Router. Going through
// Router() is the point: a test that mounts its own copy of the compression
// middleware would prove the middleware works and nothing about whether the
// daemon installs it.
func routerForTest(t *testing.T) http.Handler {
	t.Helper()
	dbh, err := db.Open(t.TempDir() + "/c.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pool := clientpool.New()
	pool.Reload(config.Config{})
	s := &Server{
		db:      dbh,
		pool:    pool,
		store:   store,
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s.Router()
}

// TestJSONResponsesAreCompressed is the wiring half of the payload finding.
//
// /scenes/{id}/releases now carries the verifier's reasoning for up to
// maxExplainedReleases releases, which is a few hundred kilobytes of highly
// repetitive prose, and it was going out uncompressed: the router installed
// Recoverer and nothing else. The size budget in match_explain_size_test.go
// measures gzip's effect on that body; this asserts the daemon actually applies
// it.
func TestJSONResponsesAreCompressed(t *testing.T) {
	r := routerForTest(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status %d: %s", rec.Code, rec.Body.String())
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip: JSON responses are going out uncompressed", enc)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is labelled gzip but does not decompress: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(plain, &body); err != nil {
		t.Fatalf("decompressed body is not JSON: %v", err)
	}

	// A client that did not ask for gzip must still get plain JSON.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q for a client that did not offer gzip", enc)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("uncompressed body is not JSON: %v", err)
	}
}

// TestSPAIsNotCompressed guards the reason compression is restricted to
// application/json.
//
// serveUI answers 304 off a strong ETag computed over the UNCOMPRESSED bundle.
// Compressing that response would leave a strong validator describing content
// the client did not receive, and this repo has already been burned once by
// browsers serving a stale SPA (hence the no-cache + ETag pair). Restricting
// the middleware by content type is what keeps the two apart, so it is asserted
// rather than assumed.
func TestSPAIsNotCompressed(t *testing.T) {
	r := routerForTest(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("SPA status %d", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("SPA Content-Encoding = %q; the ETag is computed over the uncompressed bundle", enc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("SPA has no ETag")
	}

	// And the revalidation it exists for still answers 304 through the full
	// middleware chain, not just the bare handler.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation through the router: code=%d len=%d, want 304 empty", rec.Code, rec.Body.Len())
	}
}
