package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/watches"
)

func TestChooseFailover(t *testing.T) {
	mk := func(indexer, url string, verified, rejected bool, seeders int, protocol string) sceneRelease {
		return sceneRelease{
			Title: "Rel " + indexer, Indexer: indexer, Protocol: protocol,
			DownloadURL: url, Verified: verified, Rejected: rejected, Seeders: seeders,
		}
	}
	ranked := []sceneRelease{
		mk("FailedIdx", "http://p/dl/1", true, false, 50, "torrent"), // the release that failed
		mk("Unverified", "http://p/dl/2", false, false, 90, "torrent"),
		mk("RejectRuled", "http://p/dl/3", true, true, 90, "torrent"),
		mk("DeadSeeds", "http://p/dl/4", true, false, 0, "torrent"),
		mk("UsenetAlt", "http://p/dl/5", true, false, 0, "usenet"),
		mk("BenchedIdx", "http://p/dl/6", true, false, 40, "torrent"),
		mk("GoodAlt", "http://p/dl/7", true, false, 30, "torrent"),
		mk("LaterAlt", "http://p/dl/8", true, false, 99, "torrent"),
	}
	disabled := map[string]bool{"benchedidx": true}

	got := chooseFailover(ranked, "FailedIdx", "http://p/dl/1", "torrent", disabled)
	if got == nil || got.Indexer != "GoodAlt" {
		t.Fatalf("chooseFailover = %+v, want GoodAlt (first verified grabbable torrent on a healthy other indexer)", got)
	}

	// Same indexer under a different URL is still skipped (case-insensitive).
	got = chooseFailover([]sceneRelease{
		mk("failedidx", "http://p/dl/9", true, false, 10, "torrent"),
	}, "FailedIdx", "http://p/dl/1", "torrent", nil)
	if got != nil {
		t.Fatalf("release from the failed indexer must be skipped, got %+v", got)
	}

	// No qualifying alternative: nil.
	if got := chooseFailover(nil, "X", "", "torrent", nil); got != nil {
		t.Fatalf("empty list must return nil, got %+v", got)
	}
}

// deferOrFailGrab stamps fail_kind from the typed error so the retry
// loop knows whether failover applies.
func TestDeferRecordsFailKind(t *testing.T) {
	s := newDeferTestServer(t)
	ctx := context.Background()

	fetchErr := fmt.Errorf("torrent add: fetch torrent 429: slow down (%w) (%w)",
		clienterr.ErrTransient, qbit.ErrIndexerFetch)
	clientErr := fmt.Errorf("qbit request: dial tcp: connection refused (%w)", clienterr.ErrTransient)

	idA := insertQueuedGrab(t, s, "qbit")
	s.deferOrFailGrab(ctx, idA, "torrent add: fetch 429", fetchErr)
	if g, _ := s.grabs.Get(ctx, idA); g.FailKind != "indexer" {
		t.Fatalf("fetch failure: fail_kind = %q, want indexer", g.FailKind)
	}

	idB := insertQueuedGrab(t, s, "qbit")
	s.deferOrFailGrab(ctx, idB, "torrent add: refused", clientErr)
	if g, _ := s.grabs.Get(ctx, idB); g.FailKind != "client" {
		t.Fatalf("client failure: fail_kind = %q, want client", g.FailKind)
	}
}

// The retry loop switches an indexer-failed grab to the stubbed failover
// pick before re-driving: row mutated in place (scene linkage kept), old
// client link cleared, and the visible reason names the new indexer.
func TestTickDeferredRetriesFailsOver(t *testing.T) {
	added := make(chan string, 2)
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

	alt := sceneRelease{
		Title:       "Better Release",
		Indexer:     "HealthyIdx",
		Protocol:    "torrent",
		Seeders:     42,
		Size:        123456,
		Verified:    true,
		DownloadURL: "magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	var sawGrab grabs.Grab
	s.resolveFailover = func(_ context.Context, g *grabs.Grab) *sceneRelease {
		sawGrab = *g
		return &alt
	}

	ctx := context.Background()
	id, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle:       "Original Release",
		ReleaseIndexer:     "DeadIdx",
		Client:             "qbit",
		DownloadURL:        "http://prowlarr/download/original.torrent",
		PredictedStashDBID: "scene-123",
		PerformerName:      "Performer X",
	})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "deferred"
	g.Attempts = 2 // failover is single-shot, firing exactly on the retry after the second failure
	g.FailKind = "indexer"
	g.NextRetryAt = time.Now().Add(-time.Minute).Unix()
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}

	s.tickDeferredRetries(ctx)

	if sawGrab.PredictedStashDBID != "scene-123" || sawGrab.ReleaseIndexer != "DeadIdx" {
		t.Fatalf("resolver saw %+v, want the grab's scene + failed indexer", sawGrab)
	}
	g, _ = s.grabs.Get(ctx, id)
	if g.Status != "queued" {
		t.Fatalf("status = %s, want queued", g.Status)
	}
	if g.ReleaseIndexer != "HealthyIdx" || g.ReleaseTitle != "Better Release" ||
		g.DownloadURL != alt.DownloadURL || g.ReleaseSize != alt.Size {
		t.Fatalf("release not switched: %+v", g)
	}
	if g.PredictedStashDBID != "scene-123" {
		t.Fatal("scene linkage lost on failover")
	}
	if !strings.Contains(g.Reason, "failed over to HealthyIdx (attempt 3/5)") {
		t.Fatalf("reason = %q, want the failover story", g.Reason)
	}
	// The re-driven add carried the NEW release's magnet.
	select {
	case urls := <-added:
		if !strings.Contains(urls, "btih:aaaa") {
			t.Fatalf("re-add used the wrong target: %q", urls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failover add never reached qbit")
	}
}

// No alternative: the original release retries unchanged.
func TestTickDeferredRetriesFailoverFallsBackToOriginal(t *testing.T) {
	qbitSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/torrents/add") {
			_, _ = w.Write([]byte("Ok."))
			return
		}
		_, _ = w.Write([]byte("v5.1.4"))
	}))
	defer qbitSrv.Close()

	s := newDeferTestServer(t)
	s.pool.Reload(config.Config{QbitURL: qbitSrv.URL})
	s.resolveFailover = func(context.Context, *grabs.Grab) *sceneRelease { return nil }

	ctx := context.Background()
	id := insertQueuedGrab(t, s, "qbit")
	g, _ := s.grabs.Get(ctx, id)
	g.Status = "deferred"
	g.Attempts = 2
	g.FailKind = "indexer"
	g.NextRetryAt = time.Now().Add(-time.Minute).Unix()
	orig := g.DownloadURL
	if err := s.grabs.Update(ctx, *g); err != nil {
		t.Fatal(err)
	}

	s.tickDeferredRetries(ctx)
	g, _ = s.grabs.Get(ctx, id)
	if g.Status != "queued" || g.DownloadURL != orig {
		t.Fatalf("fallback retry mutated the release: status=%s url=%s", g.Status, g.DownloadURL)
	}
}

// disabledIndexers caches Prowlarr's benched-indexer set for the TTL and
// maps ids to lowercase names.
func TestDisabledIndexersCache(t *testing.T) {
	var statusHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/indexerstatus":
			statusHits++
			_, _ = w.Write([]byte(`[{"indexerId": 5, "disabledTill": "2099-01-01T00:00:00Z"},
				{"indexerId": 6, "disabledTill": "2001-01-01T00:00:00Z"}]`))
		case "/api/v1/indexer":
			_, _ = w.Write([]byte(`[{"id": 5, "name": "XXXClub", "protocol": "torrent", "enable": true},
				{"id": 6, "name": "Recovered", "protocol": "torrent", "enable": true}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := newDeferTestServer(t)
	s.pool.Reload(config.Config{ProwlarrURL: srv.URL, ProwlarrAPIKey: "k"})

	got := s.disabledIndexers(context.Background())
	if !got["xxxclub"] {
		t.Fatalf("XXXClub (future disabledTill) missing from %v", got)
	}
	if got["recovered"] {
		t.Fatal("an expired disabledTill must not bench the indexer")
	}
	// Second read within the TTL serves the cache.
	_ = s.disabledIndexers(context.Background())
	if statusHits != 1 {
		t.Fatalf("indexerstatus fetched %d times within TTL, want 1", statusHits)
	}
}

// The watch auto-pick consults Prowlarr's bench list: when the recorded
// best release's indexer is benched, the grab switches to the best
// healthy stored candidate instead of burning a defer cycle. An
// explicit candidate re-pick is never adjusted (not tested here; that
// path takes the user's URL verbatim).
func TestGrabAvailableWatchSkipsBenchedIndexer(t *testing.T) {
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
	prowlarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/indexerstatus":
			_, _ = w.Write([]byte(`[{"indexerId": 9, "disabledTill": "2099-01-01T00:00:00Z"}]`))
		case "/api/v1/indexer":
			_, _ = w.Write([]byte(`[{"id": 9, "name": "BenchedIdx", "protocol": "torrent", "enable": true}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer prowlarrSrv.Close()

	s := newDeferTestServer(t)
	s.pool.Reload(config.Config{
		QbitURL:        qbitSrv.URL,
		ProwlarrURL:    prowlarrSrv.URL,
		ProwlarrAPIKey: "k",
	})
	ctx := context.Background()

	cands, _ := json.Marshal([]sceneRelease{
		{Title: "Benched Best", Indexer: "BenchedIdx", Protocol: "torrent", Seeders: 90,
			Verified: true, DownloadURL: "magnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Title: "Healthy Alt", Indexer: "GoodIdx", Protocol: "torrent", Seeders: 40,
			Verified: true, DownloadURL: "magnet:?xt=urn:btih:cccccccccccccccccccccccccccccccccccccccc"},
	})
	if err := s.watches.Add(ctx, watches.Watch{StashDBID: "sc1", Title: "Scene", Target: watches.TargetAny}); err != nil {
		t.Fatal(err)
	}
	if err := s.watches.MarkAvailable(ctx, "sc1", "Benched Best",
		"magnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "BenchedIdx", "torrent", 1, cands); err != nil {
		t.Fatal(err)
	}

	if err := s.grabAvailableWatch(ctx, "sc1"); err != nil {
		t.Fatalf("grab: %v", err)
	}

	select {
	case urls := <-added:
		if !strings.Contains(urls, "btih:cccc") {
			t.Fatalf("grab used the benched pick, want the healthy alternative: %q", urls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("add never reached qbit")
	}
	// The watch records the release actually grabbed.
	wt := s.findWatch(ctx, "sc1")
	if wt == nil || wt.FoundIndexer != "GoodIdx" {
		t.Fatalf("watch found_* not updated to the grabbed release: %+v", wt)
	}
}
