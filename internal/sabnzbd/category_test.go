package sabnzbd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

func sabCategoryServer(t *testing.T, body string, seen *url.Values) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = r.URL.Query()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEnsureCategoryWritesConfig: SAB has no create-category call — categories
// are config, written through mode=set_config with section=categories, and a
// keyword that doesn't exist yet is created. Pin the exact query, since
// getting any part of it wrong silently writes nothing useful.
func TestEnsureCategoryWritesConfig(t *testing.T) {
	var seen url.Values
	srv := sabCategoryServer(t, `{"status": true}`, &seen)
	c := New(srv.URL, "key")

	if err := c.EnsureCategory(context.Background(), "forage", "/data/media/downloads/complete"); err != nil {
		t.Fatalf("EnsureCategory: %v", err)
	}
	for k, want := range map[string]string{
		"mode":    "set_config",
		"section": "categories",
		"keyword": "forage",
		"dir":     "/data/media/downloads/complete",
	} {
		if got := seen.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestEnsureCategoryDetectsRefusal: SAB answers HTTP 200 with {"status": false}
// when it rejects a config write, so a transport-level success is not enough to
// conclude the category exists. Without this the UI would report "ready" for a
// category SAB never created.
func TestEnsureCategoryDetectsRefusal(t *testing.T) {
	var seen url.Values
	srv := sabCategoryServer(t, `{"status": false}`, &seen)
	c := New(srv.URL, "key")

	if err := c.EnsureCategory(context.Background(), "forage", "/path"); err == nil {
		t.Fatal("a 200 with status:false must be reported as a failure")
	}
}

// TestEnsureCategoryRejectsEmptyDir: passing dir="" through set_config would
// BLANK an existing category's folder — a destructive no-op dressed as a save.
// Refuse instead.
func TestEnsureCategoryRejectsEmptyDir(t *testing.T) {
	var seen url.Values
	srv := sabCategoryServer(t, `{"status": true}`, &seen)
	c := New(srv.URL, "key")

	if err := c.EnsureCategory(context.Background(), "forage", ""); err == nil {
		t.Fatal("expected an error for an empty folder")
	}
	if seen.Get("mode") != "" {
		t.Errorf("request was sent (%v); an empty folder must not reach SAB", seen)
	}
}
