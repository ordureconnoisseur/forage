package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/configstore"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/paniclog"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// TestDiagBundle covers the two properties the bundle exists for: it
// carries enough to triage (version, config sources, panic with stack) and
// it NEVER carries a secret value — the mask must hold even though the
// config section includes fields whose composed values are set.
func TestDiagBundle(t *testing.T) {
	dbh, err := db.Open(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	store, err := configstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-verysecret-apikey-1234"
	stashURL := "http://stash.local:9999"
	if err := store.Set(configstore.Patch{StashURL: &stashURL, StashAPIKey: strptr(secret)}); err != nil {
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
		version: "test-sha",
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// A persisted panic must surface whole, stack included.
	paniclog.Record(dbh, "poller tick", "runtime error: index out of range", []byte("goroutine 1 [running]:\nmain.tick()"))

	rec := httptest.NewRecorder()
	s.getDiag(rec, httptest.NewRequest(http.MethodGet, "/diag", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	raw := rec.Body.String()
	if strings.Contains(raw, secret) {
		t.Fatalf("diag bundle leaked a secret value:\n%s", raw)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["version"] != "test-sha" {
		t.Errorf("version = %v", body["version"])
	}
	for _, key := range []string{"goVersion", "os", "arch", "config", "clients", "grabTotals", "destructions"} {
		if _, ok := body[key]; !ok {
			t.Errorf("bundle missing %q", key)
		}
	}
	// The non-secret URL should be present (it's what setup bugs hinge on);
	// the secret field should be flagged set-but-masked.
	fields := body["config"].(map[string]any)
	if f := fields["stashUrl"].(map[string]any); f["value"] != stashURL {
		t.Errorf("stashUrl value = %v, want %v", f["value"], stashURL)
	}
	if f := fields["stashApiKey"].(map[string]any); f["hasSecret"] != true || f["value"] != "" {
		t.Errorf("stashApiKey = %v, want masked with hasSecret", f)
	}
	pe := body["lastPanic"].(map[string]any)
	if pe["in"] != "poller tick" || !strings.Contains(pe["stack"].(string), "main.tick") {
		t.Errorf("lastPanic = %v", pe)
	}
}

func strptr(s string) *string { return &s }
