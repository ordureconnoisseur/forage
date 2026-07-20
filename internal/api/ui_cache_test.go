package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The single-file SPA must never be heuristically cached (that left
// browsers on days-old bundles after deploys): every load revalidates
// via no-cache, and an unchanged build answers 304 off the ETag.
func TestServeUICacheHeaders(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.serveUI(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("first load: code=%d etag=%q len=%d", rec.Code, etag, rec.Body.Len())
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", etag)
	s.serveUI(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("revalidation: code=%d len=%d, want 304 empty", rec.Code, rec.Body.Len())
	}
}
