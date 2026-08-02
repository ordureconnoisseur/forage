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
	"github.com/ordureconnoisseur/forager/internal/invariants"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

func invariantServer(t *testing.T, rep *invariants.Report) *Server {
	t.Helper()
	dbh, err := db.Open(t.TempDir() + "/inv.db")
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
	if rep != nil {
		s.invariants = func() *invariants.Report { return rep }
	}
	return s
}

func sampleReport() *invariants.Report {
	return &invariants.Report{
		RanAt:      1750000000,
		DurationMs: 12,
		Violations: 3,
		Failing:    []string{"grab.placed_path_missing"},
		Results: []invariants.Result{
			{
				Name:      "grab.placed_path_missing",
				Statement: "a grab recording a placed file has that file on disk",
				Count:     3,
				Scanned:   500,
				Samples: []invariants.Violation{
					{Kind: "grab", ID: "812", Detail: "placed_path /data/porn/Media/Unsorted/thing.mp4 is not on disk"},
				},
			},
			{
				Name:      "grab.confirmed_scene_gone_from_stash",
				Statement: "a grab confirmed against a scene still has that scene in Stash",
				Skipped:   "stash not configured",
			},
		},
	}
}

// /healthz carries the digest, and ONLY the digest. The endpoint is
// unauthenticated (the plugin probes it before any credential is entered),
// while the samples quote library paths and release titles.
func TestHealthzCarriesInvariantDigestWithoutDetail(t *testing.T) {
	s := invariantServer(t, sampleReport())
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	body := rec.Body.String()
	if strings.Contains(body, "/data/porn") || strings.Contains(body, "thing.mp4") {
		t.Fatalf("healthz leaked a sample path to an unauthenticated caller: %s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	inv, ok := payload["invariants"].(map[string]any)
	if !ok {
		t.Fatalf("healthz has no invariants block: %s", body)
	}
	if inv["violations"] != float64(3) {
		t.Errorf("violations = %v, want 3", inv["violations"])
	}
	failing, _ := inv["failing"].(map[string]any)
	if failing["grab.placed_path_missing"] != float64(3) {
		t.Errorf("digest must name the failing invariant and its count, got %v", inv["failing"])
	}
	// A skipped check is not a passing one. Without this a daemon whose Stash
	// has been unconfigured for a month reads as fully verified.
	notChecked, _ := inv["notChecked"].([]any)
	if len(notChecked) != 1 || notChecked[0] != "grab.confirmed_scene_gone_from_stash" {
		t.Errorf("digest must name checks that did not run, got %v", inv["notChecked"])
	}
}

// A daemon with no checker wired (tests, and any future build without the
// poller) must still serve /healthz rather than panic on the nil hook.
func TestHealthzWithoutCheckerOmitsBlock(t *testing.T) {
	s := invariantServer(t, nil)
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["invariants"]; present {
		t.Error("invariants block present with no checker attached")
	}
}

// The authenticated endpoint carries the rows and what is wrong with them:
// the part that makes a violation actionable instead of merely alarming.
func TestGetInvariantsReturnsSamples(t *testing.T) {
	s := invariantServer(t, sampleReport())
	rec := httptest.NewRecorder()
	s.getInvariants(rec, httptest.NewRequest(http.MethodGet, "/invariants", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got invariants.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 || len(got.Results[0].Samples) != 1 {
		t.Fatalf("samples missing from the authenticated report: %+v", got.Results)
	}
	if got.Results[0].Samples[0].ID != "812" {
		t.Errorf("sample must name the row to act on, got %+v", got.Results[0].Samples[0])
	}
}

// Before the first pass the endpoint says so rather than serving an empty
// report: "not measured yet" and "measured, nothing wrong" must not look
// alike to whoever is triaging.
func TestGetInvariantsBeforeFirstPass(t *testing.T) {
	s := invariantServer(t, nil)
	s.invariants = func() *invariants.Report { return nil }
	rec := httptest.NewRecorder()
	s.getInvariants(rec, httptest.NewRequest(http.MethodGet, "/invariants", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
