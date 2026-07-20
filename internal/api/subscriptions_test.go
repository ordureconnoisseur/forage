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
		"torrent add: qbit declined this torrent — it's a valid .torrent but qbit/libtorrent wouldn't add it",
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

// Upgrade-watch semantics: only releases BEATING the floor qualify, and
// reconcile treats owned-below-floor as "keep hunting" rather than done.
func TestUpgradeWatchFloorGate(t *testing.T) {
	s := newDeferTestServer(t)
	cands := []sceneRelease{
		{Title: "Scene.Name.720p.MP4", Indexer: "A", Protocol: "torrent", Seeders: 50,
			Verified: true, DownloadURL: "http://x/720"},
		{Title: "Scene.Name.1080p.MP4", Indexer: "B", Protocol: "torrent", Seeders: 40,
			Verified: true, DownloadURL: "http://x/1080"},
	}
	// Floor 720: only the 1080p qualifies.
	got := s.bestWatchMatch(cands, nil, 720)
	if got == nil || got.DownloadURL != "http://x/1080" {
		t.Fatalf("floor 720: want the 1080p release, got %+v", got)
	}
	// Floor 1080: nothing beats it.
	if got := s.bestWatchMatch(cands, nil, 1080); got != nil {
		t.Fatalf("floor 1080: want no match, got %+v", got)
	}
	// Floor 0 (acquire watch): best release wins as before.
	if got := s.bestWatchMatch(cands, nil, 0); got == nil {
		t.Fatal("acquire watch must still match")
	}
}

// Reconcile must not graduate an upgrade watch just because the scene is
// owned (that is its starting condition); it graduates when a copy
// exceeds the floor.
func TestReconcileUpgradeWatchInvariant(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	s.ownedCopies = map[string][]stash.SceneRef{
		"up-pending": {{SceneID: "l1", Height: 720}},
		"up-done":    {{SceneID: "l2", Height: 720}, {SceneID: "l3", Height: 1080}},
	}
	s.ownedCopiesFetched = time.Now()

	for _, sid := range []string{"up-pending", "up-done"} {
		if err := s.watches.Add(ctx, watches.Watch{
			StashDBID: sid, Title: sid, UpgradeFloor: 720,
		}); err != nil {
			t.Fatal(err)
		}
		// Simulate a grabbed upgrade watch (upgrade grab was fired earlier).
		if err := s.watches.MarkGrabbed(ctx, sid, "T", "http://u", "I", "torrent", 1); err != nil {
			t.Fatal(err)
		}
	}

	s.reconcileWatches(ctx)

	pending := s.findWatch(ctx, "up-pending")
	if pending == nil || pending.Status != watches.StatusWatching {
		t.Fatalf("owned-below-floor upgrade watch must revert to hunting, got %+v", pending)
	}
	done := s.findWatch(ctx, "up-done")
	if done == nil || done.Status != watches.StatusGrabbed {
		t.Fatalf("owned-above-floor upgrade watch must stay done, got %+v", done)
	}
}

// The creation endpoint watches exactly the owned-below-cutoff scenes.
func TestPostUpgradeWatchesFilters(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	s.ownedCopies = map[string][]stash.SceneRef{
		"sc-720":  {{SceneID: "a", Height: 720}},
		"sc-2160": {{SceneID: "b", Height: 2160}},
	}
	s.ownedCopiesFetched = time.Now()
	s.subScenes = func(context.Context, string, string) ([]stashdb.Scene, error) {
		return []stashdb.Scene{
			{ID: "sc-720", Title: "Owned 720p", Date: "2020-01-01"},
			{ID: "sc-2160", Title: "Owned 4K", Date: "2020-01-02"},
			{ID: "sc-missing", Title: "Not Owned", Date: "2020-01-03"},
		}, nil
	}

	body := strings.NewReader(`{"kind":"performer","stashdb_id":"perf-x","name":"Perf X","cutoff":1080}`)
	req := httptest.NewRequest(http.MethodPost, "/watches/upgrade", body)
	rec := httptest.NewRecorder()
	s.postUpgradeWatches(rec, req)

	var out struct {
		Created int `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.Created != 1 {
		t.Fatalf("created = %d, want 1 (only the owned-720p scene)", out.Created)
	}
	wt := s.findWatch(ctx, "sc-720")
	if wt == nil || wt.UpgradeFloor != 720 || wt.BatchID != "upg:perf-x" {
		t.Fatalf("upgrade watch wrong: %+v", wt)
	}
	if s.findWatch(ctx, "sc-2160") != nil || s.findWatch(ctx, "sc-missing") != nil {
		t.Fatal("at-cutoff and unowned scenes must not be watched for upgrades")
	}
}

// Discover content filters: parsing, dormancy when unconfigured, and
// gender-set scene filtering.
func TestDiscoverContentFilters(t *testing.T) {
	// Parser: named sets, uppercased, malformed entries skipped.
	f := parseDiscoverFilters("Trans=transgender_female, TRANSGENDER_MALE; Broken; Fem=FEMALE")
	if len(f) != 2 || len(f["Trans"]) != 2 || f["Trans"][0] != "TRANSGENDER_FEMALE" {
		t.Fatalf("parse = %v", f)
	}
	if parseDiscoverFilters("") == nil || len(parseDiscoverFilters("")) != 0 {
		t.Fatal("unset config must yield an empty (dormant) map")
	}

	scenes := []discoverScene{
		{StashDBID: "s1", Performers: []discoverPerformer{{Name: "A", Gender: "TRANSGENDER_FEMALE"}}},
		{StashDBID: "s2", Performers: []discoverPerformer{{Name: "B", Gender: "FEMALE"}}},
		{StashDBID: "s3", Performers: []discoverPerformer{{Name: "C", Gender: "FEMALE"}, {Name: "D", Gender: "TRANSGENDER_MALE"}}},
		{StashDBID: "s4", Performers: nil}, // no local performers: unjudgeable, dropped
	}
	got := filterScenesByGender(scenes, f["Trans"])
	if len(got) != 2 || got[0].StashDBID != "s1" || got[1].StashDBID != "s3" {
		t.Fatalf("filter kept %v", got)
	}
}

// Subscribing resolves whatever id the caller passed (the performer
// page's LOCAL navigation id, or an actual cross-id) to the StashDB
// cross-id the loop matches by, keeps the local id for the UI, and
// refuses performers with no StashDB link outright.
func TestPostSubscriptionResolvesPerformerIDs(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`
		INSERT INTO performer_cache (stash_id, stashdb_id, name, refreshed_at)
		VALUES ('77', 'uuid-abc-1', 'Linked', 1), ('88', '', 'Unlinked', 1)`); err != nil {
		t.Fatal(err)
	}

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.postSubscription(rec, httptest.NewRequest(http.MethodPost, "/subscriptions", strings.NewReader(body)))
		return rec
	}

	// Local id in: cross-id stored, local id kept.
	if rec := post(`{"stashdb_id":"77","kind":"performer","name":"Linked"}`); rec.Code != http.StatusOK {
		t.Fatalf("local-id subscribe = %d: %s", rec.Code, rec.Body.String())
	}
	subs, _ := s.subs.List(ctx)
	if len(subs) != 1 || subs[0].StashDBID != "uuid-abc-1" || subs[0].LocalID != "77" {
		t.Fatalf("stored sub = %+v, want cross-id key + local id", subs)
	}

	// Cross-id in: same row updated (upsert), local id backfilled.
	if rec := post(`{"stashdb_id":"uuid-abc-1","kind":"performer","name":"Linked"}`); rec.Code != http.StatusOK {
		t.Fatalf("cross-id subscribe = %d", rec.Code)
	}
	if subs, _ = s.subs.List(ctx); len(subs) != 1 || subs[0].LocalID != "77" {
		t.Fatalf("cross-id subscribe must merge into the same row: %+v", subs)
	}

	// No StashDB link: refused, not stored as a dud that never fires.
	if rec := post(`{"stashdb_id":"88","kind":"performer","name":"Unlinked"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unlinked subscribe = %d, want 422", rec.Code)
	}
	if subs, _ = s.subs.List(ctx); len(subs) != 1 {
		t.Fatalf("unlinked subscribe must not store: %+v", subs)
	}
}

// The loop self-heals legacy performer subscriptions stored under a
// LOCAL Stash id (they could never match aggregates or scenes): rekeyed
// to the cross-id with the local id preserved, watermark intact.
func TestSubscriptionTickRekeysLocalIDSubs(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`
		INSERT INTO performer_cache (stash_id, stashdb_id, name, refreshed_at)
		VALUES ('1206', 'uuid-reina', 'Reina', 1)`); err != nil {
		t.Fatal(err)
	}
	// Legacy row: keyed by the local id, as old plugin builds subscribed.
	if err := s.subs.Add(ctx, subscriptions.Subscription{
		StashDBID: "1206", Kind: "performer", Name: "Reina Ohara",
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.subs.List(ctx)

	s.tickSubscriptions(ctx)

	after, err := s.subs.List(ctx)
	if err != nil || len(after) != 1 {
		t.Fatalf("subs after tick: %v %v", after, err)
	}
	if after[0].StashDBID != "uuid-reina" || after[0].LocalID != "1206" {
		t.Fatalf("sub not rekeyed: %+v", after[0])
	}
	if after[0].Watermark != before[0].Watermark {
		t.Fatalf("rekey must preserve the watermark: %d != %d", after[0].Watermark, before[0].Watermark)
	}
}
