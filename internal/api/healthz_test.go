package api

import (
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
)

// TestHealthzReachabilityDefaults verifies the reachability fields the UI
// banner keys off are always present and default safely: with no download
// client configured, both clients report reachable and clientErrors is an
// empty JSON array (never null, never a spurious failure). The active-probe
// failure path is covered directly in clienthealth_test.go — exercising it
// here would require a live-but-dead client and a real probe timeout.
func TestHealthzReachabilityDefaults(t *testing.T) {
	dbh, err := db.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
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

	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode healthz: %v (body=%s)", err, rec.Body.String())
	}

	for _, key := range []string{"qbitReachable", "sabReachable", "clientErrors"} {
		if _, ok := body[key]; !ok {
			t.Errorf("healthz payload missing %q", key)
		}
	}
	if body["qbitReachable"] != true {
		t.Errorf("qbitReachable = %v, want true when unconfigured", body["qbitReachable"])
	}
	if body["sabReachable"] != true {
		t.Errorf("sabReachable = %v, want true when unconfigured", body["sabReachable"])
	}
	// clientErrors must be an empty array, not null — the UI iterates it.
	errs, ok := body["clientErrors"].([]any)
	if !ok {
		t.Fatalf("clientErrors = %#v, want []any", body["clientErrors"])
	}
	if len(errs) != 0 {
		t.Errorf("clientErrors = %v, want empty when no client configured", errs)
	}
}
