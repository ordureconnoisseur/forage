package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
	"github.com/ordureconnoisseur/forager/internal/subscriptions"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

// seedPerformerAggregate plants the count-sync signal the loop's change
// detection reads.
func seedPerformerAggregate(t *testing.T, s *Server, stashdbID string, lastRelease int64) {
	t.Helper()
	if _, err := s.db.Exec(`
		INSERT INTO performer_cache (stash_id, stashdb_id, name, last_release_unix, refreshed_at)
		VALUES (?, ?, 'Seeded', ?, 1)`, "local-"+stashdbID, stashdbID, lastRelease); err != nil {
		t.Fatal(err)
	}
}

func day(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

// One pass: a subscribed performer with one scene newer than the
// watermark, one older, and one already watched. Exactly one watch is
// created, batch-tagged to the subscription, and the watermark advances
// to the newest date seen.
func TestSubscriptionTickCreatesWatchesForNewScenes(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()

	if err := s.subs.Add(ctx, subscriptions.Subscription{
		StashDBID: "perf-1", Kind: "performer", Name: "Kenzie Reeves",
	}); err != nil {
		t.Fatal(err)
	}
	// The repo initialised the watermark to today; move it back so the
	// fixture dates below are unambiguous.
	subsList, _ := s.subs.List(ctx)
	wm := day("2026-07-01").Unix()
	if _, err := s.db.Exec(`UPDATE subscriptions SET watermark = ? WHERE stashdb_id = 'perf-1'`, wm); err != nil {
		t.Fatal(err)
	}
	_ = subsList
	seedPerformerAggregate(t, s, "perf-1", day("2026-07-10").Unix())
	// Pre-warm the owned-copies memo: the harness has no Stash, and the
	// creation phase deliberately skips when the owned sweep errors.
	s.ownedCopies = map[string][]stash.SceneRef{}
	s.ownedCopiesFetched = time.Now()

	// A pre-existing watch for one of the "new" scenes: must not be
	// re-added (watches.Add upserts and would RESET it to watching).
	if err := s.watches.Add(ctx, watches.Watch{StashDBID: "scene-watched", Title: "Already Watched"}); err != nil {
		t.Fatal(err)
	}

	s.subScenes = func(_ context.Context, kind, id string) ([]stashdb.Scene, error) {
		if kind != "performer" || id != "perf-1" {
			t.Errorf("scene fetch for wrong subject: %s/%s", kind, id)
		}
		return []stashdb.Scene{
			{ID: "scene-old", Title: "Backlog Scene", Date: "2026-06-01"},
			{ID: "scene-new", Title: "Fresh Scene", Date: "2026-07-10"},
			{ID: "scene-watched", Title: "Already Watched", Date: "2026-07-09"},
		}, nil
	}

	s.tickSubscriptions(ctx)

	list, err := s.watches.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var created []watches.Watch
	for _, wt := range list {
		if wt.BatchID == subscriptions.BatchPrefix+"perf-1" {
			created = append(created, wt)
		}
	}
	if len(created) != 1 || created[0].StashDBID != "scene-new" {
		t.Fatalf("want exactly scene-new watched under the sub batch, got %+v", created)
	}
	if created[0].PerformerName != "Kenzie Reeves" || created[0].BatchLabel != "Kenzie Reeves" {
		t.Fatalf("watch missing subscription identity: %+v", created[0])
	}

	subsList, _ = s.subs.List(ctx)
	if subsList[0].Watermark != day("2026-07-10").Unix() {
		t.Fatalf("watermark = %d, want advanced to newest seen date", subsList[0].Watermark)
	}

	// Second pass with unchanged aggregates: no duplicates, no resets.
	s.subScenes = func(context.Context, string, string) ([]stashdb.Scene, error) {
		t.Error("aggregates unchanged: scene fetch must not run")
		return nil, nil
	}
	s.tickSubscriptions(ctx)
}

// An auto-grab subscription grabs its available watches through the
// normal watch-grab path.
func TestSubscriptionAutoGrabsAvailableWatches(t *testing.T) {
	added := make(chan string, 1)
	qbitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/torrents/add") {
			_ = r.ParseMultipartForm(1 << 20)
			select {
			case added <- r.FormValue("urls"):
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

	if err := s.subs.Add(ctx, subscriptions.Subscription{
		StashDBID: "perf-2", Kind: "performer", Name: "Auto Performer",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.subs.SetAutoGrab(ctx, "perf-2", true); err != nil {
		t.Fatal(err)
	}
	seedPerformerAggregate(t, s, "perf-2", 0) // no new scenes; sweep-only pass
	s.subScenes = func(context.Context, string, string) ([]stashdb.Scene, error) {
		return nil, nil
	}

	// An available watch in the subscription's batch, as the watch loop
	// would have left it.
	if err := s.watches.Add(ctx, watches.Watch{
		StashDBID: "scene-avail", Title: "Ready Scene",
		BatchID: subscriptions.BatchPrefix + "perf-2", BatchLabel: "Auto Performer",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.MarkAvailable(ctx, "scene-avail", "Ready.Release",
		"magnet:?xt=urn:btih:dddddddddddddddddddddddddddddddddddddddd", "SomeIdx", "torrent", 1, nil); err != nil {
		t.Fatal(err)
	}

	s.tickSubscriptions(ctx)

	select {
	case urls := <-added:
		if !strings.Contains(urls, "btih:dddd") {
			t.Fatalf("auto-grab added the wrong target: %q", urls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("auto-grab never reached qbit")
	}
	wt := s.findWatch(ctx, "scene-avail")
	if wt == nil || wt.Status != watches.StatusGrabbed {
		t.Fatalf("watch not marked grabbed after auto-grab: %+v", wt)
	}
}

// Without auto_grab the sweep leaves available watches alone: ping-first
// is the default contract.
func TestSubscriptionPingFirstLeavesAvailableAlone(t *testing.T) {
	s := newDeferTestServer(t)
	s.pool.Reload(config.Config{QbitURL: "http://127.0.0.1:1"})
	ctx := context.Background()

	if err := s.subs.Add(ctx, subscriptions.Subscription{
		StashDBID: "perf-3", Kind: "performer", Name: "Manual Performer",
	}); err != nil {
		t.Fatal(err)
	}
	seedPerformerAggregate(t, s, "perf-3", 0)
	s.subScenes = func(context.Context, string, string) ([]stashdb.Scene, error) { return nil, nil }
	if err := s.watches.Add(ctx, watches.Watch{
		StashDBID: "scene-manual", Title: "Manual Scene",
		BatchID: subscriptions.BatchPrefix + "perf-3",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.MarkAvailable(ctx, "scene-manual", "Manual.Release",
		"magnet:?xt=urn:btih:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "Idx", "torrent", 1, nil); err != nil {
		t.Fatal(err)
	}

	s.tickSubscriptions(ctx)

	wt := s.findWatch(ctx, "scene-manual")
	if wt == nil || wt.Status != watches.StatusAvailable {
		t.Fatalf("ping-first sub must leave the available watch for the user, got %+v", wt)
	}
}
