package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/grabs"
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

// contentDeadReason separates release-is-dead failures (auto-ignore on
// revert) from infrastructure failures (same release retries fine).
func TestContentDeadReason(t *testing.T) {
	dead := []string{
		"sab: Aborted, cannot be completed - https://sabnzbd.org/not-complete",
		"sab: Repair failed, not enough repair blocks (5 short)",
		"sab: RAR files failed to verify",
		"torrent add: fetch torrent 429 (gave up after 5 attempts)",
	}
	alive := []string{
		"sab: Unpacking failed, write error or disk is full?  in the file /data/porn/downloads",
		"qbit request: dial tcp: connection refused",
		"sab status=Failed",
		"never linked to a qBit torrent (add likely failed)",
	}
	for _, r := range dead {
		if !contentDeadReason(r) {
			t.Errorf("want content-dead: %q", r)
		}
	}
	for _, r := range alive {
		if contentDeadReason(r) {
			t.Errorf("must NOT be content-dead (release is fine): %q", r)
		}
	}
}

// The reconcile reverse pass auto-ignores a content-dead release: the
// watch reverts to watching AND carries the dead URL in ignored_urls, so
// the next re-search must pick a different source. An infra-side failure
// reverts plain (same release stays eligible).
func TestReconcileAutoIgnoresDeadRelease(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	// Pre-warm owned memo (no Stash in the harness).
	s.ownedCopies = map[string][]stash.SceneRef{}
	s.ownedCopiesFetched = time.Now()

	mkFailed := func(scene, url, reason string) {
		id, err := s.grabs.Insert(ctx, grabs.Grab{
			ReleaseTitle: "Rel " + scene, Client: "sabnzbd",
			DownloadURL: url, PredictedStashDBID: scene,
		})
		if err != nil {
			t.Fatal(err)
		}
		g, _ := s.grabs.Get(ctx, id)
		g.Status = "failed"
		g.Reason = reason
		if err := s.grabs.Update(ctx, *g); err != nil {
			t.Fatal(err)
		}
	}
	mkGrabbedWatch := func(scene, url string) {
		if err := s.watches.Add(ctx, watches.Watch{StashDBID: scene, Title: "Scene " + scene}); err != nil {
			t.Fatal(err)
		}
		if err := s.watches.MarkAvailable(ctx, scene, "Rel "+scene, url, "Idx", "usenet", 1, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.watches.MarkGrabbed(ctx, scene, "Rel "+scene, url, "Idx", "usenet", 1); err != nil {
			t.Fatal(err)
		}
	}

	mkFailed("scene-dead", "http://idx/dead.nzb", "sab: Aborted, cannot be completed - see docs")
	mkGrabbedWatch("scene-dead", "http://idx/dead.nzb")
	mkFailed("scene-infra", "http://idx/infra.nzb", "sab: Unpacking failed, write error or disk is full?")
	mkGrabbedWatch("scene-infra", "http://idx/infra.nzb")

	s.reconcileWatches(ctx)

	dead := s.findWatch(ctx, "scene-dead")
	if dead == nil || dead.Status != watches.StatusWatching {
		t.Fatalf("dead-release watch must revert to watching, got %+v", dead)
	}
	found := false
	for _, u := range dead.IgnoredURLs {
		if u == "http://idx/dead.nzb" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dead release URL missing from ignored_urls: %v", dead.IgnoredURLs)
	}

	infra := s.findWatch(ctx, "scene-infra")
	if infra == nil || infra.Status != watches.StatusWatching {
		t.Fatalf("infra-failure watch must revert to watching, got %+v", infra)
	}
	if len(infra.IgnoredURLs) != 0 {
		t.Fatalf("infra failure must NOT ignore the release: %v", infra.IgnoredURLs)
	}
}

// Bulk retry-all skips content-dead grabs: re-queuing a release whose
// articles are gone just feeds the client a doomed job. (The single-grab
// Retry button deliberately has no such filter.)
func TestRetryAllSkipsContentDead(t *testing.T) {
	s := newDeferTestServer(t)
	s.pool.Reload(config.Config{QbitURL: "http://127.0.0.1:1"})
	ctx := context.Background()

	mk := func(title, reason string) {
		id, err := s.grabs.Insert(ctx, grabs.Grab{
			ReleaseTitle: title, Client: "qbit",
			DownloadURL: "magnet:?xt=urn:btih:ffffffffffffffffffffffffffffffffffff" + title[len(title)-4:],
		})
		if err != nil {
			t.Fatal(err)
		}
		g, _ := s.grabs.Get(ctx, id)
		g.Status = "failed"
		g.Reason = reason
		if err := s.grabs.Update(ctx, *g); err != nil {
			t.Fatal(err)
		}
	}
	mk("Dead.Release.aaaa", "sab: Aborted, cannot be completed - see docs")
	mk("Live.Release.bbbb", "qbit request: dial tcp: connection refused")

	req := httptest.NewRequest(http.MethodPost, "/grabs/retry-all-failed", nil)
	rec := httptest.NewRecorder()
	s.postRetryAllFailed(rec, req)

	var out struct {
		Retried     int `json:"retried"`
		ContentDead int `json:"content_dead"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ContentDead != 1 {
		t.Fatalf("content_dead = %d, want 1", out.ContentDead)
	}
	if out.Retried != 1 {
		t.Fatalf("retried = %d, want 1 (only the infra-failed grab)", out.Retried)
	}
}
