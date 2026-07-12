package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

func TestShouldDeferAdd(t *testing.T) {
	transient := fmt.Errorf("dial tcp: connection refused (%w)", clienterr.ErrTransient)
	rejected := fmt.Errorf("declined (%w)", clienterr.ErrRejected)
	notFound := fmt.Errorf("gone (%w)", clienterr.ErrNotFound)
	timeout := fmt.Errorf("addurl: %w (%w)", context.DeadlineExceeded, clienterr.ErrTransient)

	cases := []struct {
		name   string
		client string
		err    error
		want   bool
	}{
		{"qbit transient defers", "qbit", transient, true},
		{"qbit timeout defers (re-add is idempotent)", "qbit", timeout, true},
		{"qbit rejected fails", "qbit", rejected, false},
		{"qbit not-found fails", "qbit", notFound, false},
		{"sab connection-level transient defers", "sabnzbd", transient, true},
		{"sab timeout does NOT defer (may have enqueued)", "sabnzbd", timeout, false},
		{"sab rejected fails", "sabnzbd", rejected, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldDeferAdd(c.client, c.err); got != c.want {
				t.Errorf("shouldDeferAdd(%s, %v) = %v, want %v", c.client, c.err, got, c.want)
			}
		})
	}
}

func TestDeferBackoffSchedule(t *testing.T) {
	want := map[int]time.Duration{
		1: time.Minute,
		2: 5 * time.Minute,
		3: 15 * time.Minute,
		4: time.Hour,
		9: time.Hour, // later attempts stay capped
	}
	for attempts, d := range want {
		if got := deferBackoff(attempts); got != d {
			t.Errorf("deferBackoff(%d) = %v, want %v", attempts, got, d)
		}
	}
}

// newDeferTestServer builds a Server around a real repo, with the pool
// left unconfigured unless the caller Reloads it.
func newDeferTestServer(t *testing.T) *Server {
	t.Helper()
	dbh, err := db.Open(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	return &Server{
		db:      dbh,
		pool:    clientpool.New(),
		grabs:   grabs.NewRepo(dbh),
		watches: watches.NewRepo(dbh),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func insertQueuedGrab(t *testing.T, s *Server, client string) int64 {
	t.Helper()
	id, err := s.grabs.Insert(context.Background(), grabs.Grab{
		ReleaseTitle: "Test Release " + client,
		Client:       client,
		DownloadURL:  "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A transient add failure parks the grab in deferred with attempts=1 and
// a ~1m retry slot; repeated failures walk the backoff; the 5th settles
// to failed with the final error preserved.
func TestDeferOrFailGrabLifecycle(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")
	transient := fmt.Errorf("dial tcp: connection refused (%w)", clienterr.ErrTransient)

	for attempt := 1; attempt < deferMaxAttempts; attempt++ {
		s.deferOrFailGrab(ctx, id, "torrent add: refused", transient)
		g, _ := s.grabs.Get(ctx, id)
		if g.Status != "deferred" {
			t.Fatalf("attempt %d: status = %s, want deferred", attempt, g.Status)
		}
		if g.Attempts != attempt {
			t.Fatalf("attempt %d: attempts = %d", attempt, g.Attempts)
		}
		wantAt := time.Now().Add(deferBackoff(attempt)).Unix()
		if g.NextRetryAt < wantAt-5 || g.NextRetryAt > wantAt+5 {
			t.Fatalf("attempt %d: next_retry_at = %d, want ~%d", attempt, g.NextRetryAt, wantAt)
		}
		// Re-arm for the next iteration the way the retry loop would.
		g.Status = "queued"
		g.NextRetryAt = 0
		if err := s.grabs.Update(ctx, *g); err != nil {
			t.Fatal(err)
		}
	}

	// Budget exhausted: the final failure settles to failed.
	s.deferOrFailGrab(ctx, id, "torrent add: refused", transient)
	g, _ := s.grabs.Get(ctx, id)
	if g.Status != "failed" {
		t.Fatalf("after %d attempts: status = %s, want failed", deferMaxAttempts, g.Status)
	}
	if g.Attempts != deferMaxAttempts {
		t.Fatalf("attempts = %d, want %d", g.Attempts, deferMaxAttempts)
	}
	if !strings.Contains(g.Reason, "gave up") || !strings.Contains(g.Reason, "refused") {
		t.Fatalf("reason = %q, want the final error + gave-up marker", g.Reason)
	}
}

// Permanent errors bypass the defer flow entirely.
func TestDeferOrFailGrabPermanentFailsImmediately(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")

	rejected := fmt.Errorf("declined — duplicate (%w)", clienterr.ErrRejected)
	s.deferOrFailGrab(ctx, id, "torrent add: declined", rejected)
	g, _ := s.grabs.Get(ctx, id)
	if g.Status != "failed" || g.Attempts != 0 {
		t.Fatalf("rejected error: status=%s attempts=%d, want failed/0", g.Status, g.Attempts)
	}
}

// A grab the poller already advanced (the add actually landed) is never
// regressed by a late-arriving defer.
func TestDeferOrFailGrabNeverRegressesAdvancedGrab(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "downloading"
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}

	transient := fmt.Errorf("timeout (%w)", clienterr.ErrTransient)
	s.deferOrFailGrab(ctx, id, "torrent add: timeout", transient)
	g, _ = s.grabs.Get(ctx, id)
	if g.Status != "downloading" {
		t.Fatalf("status = %s, want downloading untouched", g.Status)
	}
}

// The retry loop holds a deferred grab while its client is unconfigured,
// without consuming an attempt.
func TestTickDeferredRetriesHoldsWhenClientMissing(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "deferred"
	g.Attempts = 2
	g.NextRetryAt = time.Now().Add(-time.Minute).Unix()
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}

	s.tickDeferredRetries(ctx) // pool has no qbit client
	g, _ = s.grabs.Get(ctx, id)
	if g.Status != "deferred" || g.Attempts != 2 {
		t.Fatalf("held grab mutated: status=%s attempts=%d", g.Status, g.Attempts)
	}
}

// Happy path: a due deferred grab with a reachable client is re-queued
// (attempt count preserved, schedule cleared) and the add is re-driven
// against the client.
func TestTickDeferredRetriesRequeuesDueGrab(t *testing.T) {
	added := make(chan string, 1)
	qbitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/torrents/add") {
			_ = r.ParseMultipartForm(1 << 20)
			select {
			case added <- r.URL.Path:
			default:
			}
			_, _ = w.Write([]byte("Ok."))
			return
		}
		_, _ = w.Write([]byte("v5.1.4"))
	}))
	defer qbitSrv.Close()

	s := newDeferTestServer(t)
	s.pool.Reload(config.Config{QbitURL: qbitSrv.URL})
	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "deferred"
	g.Attempts = 1
	g.Reason = "torrent add: refused"
	g.NextRetryAt = time.Now().Add(-time.Minute).Unix()
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}

	s.tickDeferredRetries(ctx)

	g, _ = s.grabs.Get(ctx, id)
	if g.Status != "queued" {
		t.Fatalf("status = %s, want queued", g.Status)
	}
	if g.Attempts != 1 {
		t.Fatalf("auto-retry must keep the attempt count, got %d", g.Attempts)
	}
	if g.NextRetryAt != 0 {
		t.Fatalf("next_retry_at = %d, want cleared", g.NextRetryAt)
	}
	if !strings.Contains(g.Reason, "attempt 2/") {
		t.Fatalf("reason = %q, want auto-retry attempt marker", g.Reason)
	}
	// The async add actually reached the client.
	select {
	case <-added:
	case <-time.After(5 * time.Second):
		t.Fatal("re-driven add never reached the qbit server")
	}
}

// A future retry slot is not picked up early.
func TestDeferredDueRespectsSchedule(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "deferred"
	g.NextRetryAt = time.Now().Add(time.Hour).Unix()
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}

	due, err := s.grabs.DeferredDue(ctx, time.Now().Unix(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("got %d due grabs, want 0 (slot is an hour away)", len(due))
	}
}

// Manual retry accepts a deferred grab and resets the attempt budget;
// auto retry refuses non-deferred grabs.
func TestRetryGrabManualVsAuto(t *testing.T) {
	s := newDeferTestServer(t)
	s.pool.Reload(config.Config{QbitURL: "http://127.0.0.1:1"})
	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "deferred"
	g.Attempts = 3
	g.NextRetryAt = time.Now().Add(time.Hour).Unix()
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}

	g, _ = s.grabs.Get(ctx, id)
	if err := s.retryGrab(ctx, g, true); err != nil {
		t.Fatalf("manual retry of deferred grab: %v", err)
	}
	fresh, _ := s.grabs.Get(ctx, id)
	if fresh.Status != "queued" || fresh.Attempts != 0 || fresh.NextRetryAt != 0 {
		t.Fatalf("manual retry: status=%s attempts=%d next=%d, want queued/0/0",
			fresh.Status, fresh.Attempts, fresh.NextRetryAt)
	}

	// Auto retry must refuse anything that isn't deferred.
	if err := s.retryGrab(ctx, fresh, false); err == nil {
		t.Fatal("auto retry of a queued grab must be refused")
	}
	fresh.Status = "failed"
	if err := s.grabs.Update(ctx, *fresh); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.grabs.Get(ctx, id)
	var ge grabError
	if err := s.retryGrab(ctx, failed, false); !errors.As(err, &ge) {
		t.Fatalf("auto retry of failed grab: err = %v, want grabError", err)
	}
}
