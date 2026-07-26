package api

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// dedupServer is a Server with only what doGrab's dedup gate touches. No
// download client is configured, so a request that gets PAST the gate fails
// loudly — which is what makes "did the gate stop it?" unambiguous here.
func dedupServer(t *testing.T) *Server {
	t.Helper()
	dbh, err := db.Open(filepath.Join(t.TempDir(), "forager.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	pool := clientpool.New()
	pool.Reload(config.Config{})
	return &Server{
		db:    dbh,
		pool:  pool,
		grabs: grabs.NewRepo(dbh),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestDoGrabDedupesCrossPostedRelease: clicking grab on a release whose
// identical file is already in the library must return the existing grab
// rather than fetching it a second time. Before this, the manual path checked
// the exact download URL only — and Prowlarr re-issues a fresh URL for the
// same release on every search, so it caught a double-click and little else.
func TestDoGrabDedupesCrossPostedRelease(t *testing.T) {
	s := dedupServer(t)
	ctx := context.Background()

	landed, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: crossPostTitleAPI, ReleaseIndexer: "NZBgeek",
		DownloadURL: "http://prowlarr/2/download?link=OLD",
		Client:      "sabnzbd", Status: "confirmed", GrabbedAt: 100,
		PlacedPath: "/lib/Liora Vane/landed.mp4",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := s.doGrab(ctx, grabRequest{
		DownloadURL:    "http://prowlarr/1/download?link=NEW",
		ReleaseTitle:   crossPostTitleAPI,
		ReleaseIndexer: "NZBFinder",
		Protocol:       "usenet",
	})
	if err != nil {
		t.Fatalf("doGrab returned an error instead of the existing grab: %v", err)
	}
	if res.GrabID != landed {
		t.Fatalf("grab_id = %d, want the existing grab %d", res.GrabID, landed)
	}
	// And no second row was created.
	all, err := s.grabs.List(ctx, "any", "", 100, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("grabs table holds %d rows, want 1 (no duplicate download queued)", len(all))
	}
}

// TestDoGrabAllowsRecoveryFromAnotherIndexer is the safety property: when the
// earlier attempt never landed, grabbing the same release from a different
// indexer is the standard fix for a dead Usenet post and must still go
// through. No client is configured here, so reaching the send path errors —
// that error IS the pass condition, since it proves the gate let it by.
func TestDoGrabAllowsRecoveryFromAnotherIndexer(t *testing.T) {
	s := dedupServer(t)
	ctx := context.Background()

	if _, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: crossPostTitleAPI, ReleaseIndexer: "NZBgeek",
		DownloadURL: "http://prowlarr/2/download?link=OLD",
		Client:      "sabnzbd", Status: "downloading", GrabbedAt: 100,
		// no PlacedPath: nothing landed, so this is a recovery attempt
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := s.doGrab(ctx, grabRequest{
		DownloadURL:    "http://prowlarr/1/download?link=NEW",
		ReleaseTitle:   crossPostTitleAPI,
		ReleaseIndexer: "NZBFinder",
		Protocol:       "usenet",
	})
	if err == nil {
		t.Fatalf("expected the grab to proceed and then fail for lack of a download client, "+
			"but it returned success (grab_id=%d) — the dedup gate blocked a recovery attempt",
			res.GrabID)
	}
}

const crossPostTitleAPI = "Bang.YNGR.26.07.10.Liora.Vane.XXX.1080p.MP4-WRB"
