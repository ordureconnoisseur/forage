package qbit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

// TestFetchTorrentBytesRetries proves a transient fetch failure (a 503,
// as Prowlarr returns mid-Cloudflare-challenge) is retried and recovers,
// rather than failing the grab on the first stall.
func TestFetchTorrentBytesRetries(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // transient
			return
		}
		_, _ = io.WriteString(w, "d4:infod...e") // pretend torrent bytes
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	body, err := c.fetchTorrentBytes(context.Background(), srv.URL+"/dl")
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if len(body) == 0 {
		t.Errorf("expected body, got empty")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 3 {
		t.Errorf("expected 3 attempts (2 transient + 1 success), got %d", hits)
	}
}

// TestFetchTorrentBytesNoRetryOn4xx confirms a 4xx (e.g. bad link) fails
// immediately without burning retries — only 5xx/timeouts are transient.
func TestFetchTorrentBytesNoRetryOn4xx(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	if _, err := c.fetchTorrentBytes(context.Background(), srv.URL+"/dl"); err == nil {
		t.Fatal("expected error on 404")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("4xx must not retry, expected 1 attempt, got %d", hits)
	}
}

// TestFetchTorrentBytesMagnetRedirect: an aggregator (Knaben) 301s its
// /download proxy to a magnet: URI. The Go client can't GET a magnet:, so the
// fetch must NOT follow it and fail — it captures the magnet from the redirect
// and returns it as the body, which the caller hands to qBit's urls field.
func TestFetchTorrentBytesMagnetRedirect(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:F5FACE375541B93C5A9BECE66FD824CCF065DF4A&dn=x"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", magnet)
		w.WriteHeader(http.StatusMovedPermanently) // 301 -> magnet, as Knaben does
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	body, err := c.fetchTorrentBytes(context.Background(), srv.URL+"/download?link=z")
	if err != nil {
		t.Fatalf("magnet redirect should resolve to the magnet, got err %v", err)
	}
	if string(body) != magnet {
		t.Errorf("body = %q, want the magnet URI", string(body))
	}
}

// TestAuthedDoReloginsOn403 is the regression guard for the bug where
// authedOnce latched true for the life of the process: once qBit expired
// the SID cookie (or restarted) mid-pack, every subsequent poll got 403
// and the grab never advanced. The client must now drop the stale
// session, log in again, and replay the request once.
func TestAuthedDoReloginsOn403(t *testing.T) {
	var mu sync.Mutex
	var loginCount, listCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			loginCount++
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session"})
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/info":
			listCount++
			// First attempt simulates an expired session.
			if listCount == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, "[]")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "pw")
	ts, err := c.ListTorrents(context.Background(), ListOpts{Filter: "all"})
	if err != nil {
		t.Fatalf("ListTorrents: %v", err)
	}
	if len(ts) != 0 {
		t.Errorf("expected 0 torrents, got %d", len(ts))
	}

	mu.Lock()
	defer mu.Unlock()
	if loginCount != 2 {
		t.Errorf("expected 2 logins (initial + relogin after 403), got %d", loginCount)
	}
	if listCount != 2 {
		t.Errorf("expected 2 list attempts (403 then retry), got %d", listCount)
	}
}

// TestAuthedDoNoRetryOnSuccess confirms the happy path doesn't log in
// twice or replay the request.
func TestAuthedDoNoRetryOnSuccess(t *testing.T) {
	var mu sync.Mutex
	var loginCount, listCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			loginCount++
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session"})
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/info":
			listCount++
			_, _ = io.WriteString(w, "[]")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "pw")
	if _, err := c.ListTorrents(context.Background(), ListOpts{Filter: "all"}); err != nil {
		t.Fatalf("ListTorrents: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if loginCount != 1 {
		t.Errorf("expected 1 login, got %d", loginCount)
	}
	if listCount != 1 {
		t.Errorf("expected 1 list attempt, got %d", listCount)
	}
}

// TestAddTorrentMagnetDuplicateRecovers pins the magnet analogue of the
// .torrent duplicate recovery: qBit refuses a duplicate magnet add with the
// same bare "Fails." it uses for a parse failure, but when the URI's btih is
// already present the add is a recoverable success — the grab links to the
// existing torrent instead of being failed.
func TestAddTorrentMagnetDuplicateRecovers(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	magnet := "magnet:?xt=urn:btih:" + hash + "&dn=Some.Release"

	var mu sync.Mutex
	var resumed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session"})
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/add":
			_, _ = io.WriteString(w, "Fails.") // duplicate refusal
		case "/api/v2/torrents/info":
			_, _ = io.WriteString(w, `[{"hash":"`+hash+`","name":"Some.Release","category":"forager","state":"uploading"}]`)
		case "/api/v2/torrents/start":
			resumed = true
			_, _ = io.WriteString(w, "")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "pw")
	got, err := c.AddTorrent(context.Background(), magnet, "forager")
	if err != nil {
		t.Fatalf("duplicate magnet add should recover, got error: %v", err)
	}
	if got != hash {
		t.Errorf("recovered hash = %q, want %q", got, hash)
	}
	mu.Lock()
	defer mu.Unlock()
	if !resumed {
		t.Errorf("expected the existing same-category torrent to be resumed")
	}
}

// TestAddTorrentMagnetGenuineParseFailure confirms a "Fails." whose hash is
// NOT in qBit still surfaces as the rejection it is.
func TestAddTorrentMagnetGenuineParseFailure(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	magnet := "magnet:?xt=urn:btih:" + hash

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session"})
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/add":
			_, _ = io.WriteString(w, "Fails.")
		case "/api/v2/torrents/info":
			_, _ = io.WriteString(w, `[]`) // hash not present — a real refusal
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "admin", "pw")
	if _, err := c.AddTorrent(context.Background(), magnet, "forager"); !errors.Is(err, clienterr.ErrRejected) {
		t.Fatalf("non-duplicate Fails. should stay ErrRejected, got: %v", err)
	}
}

// TestAddTorrentIndexerFetchSentinel verifies a failed .torrent fetch is
// classifiable as BOTH an indexer-side failure (ErrIndexerFetch, so the
// deferred-retry loop knows a failover to another indexer may rescue the
// grab) and the clienterr class the status code implies.
func TestAddTorrentIndexerFetchSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // indexer rate-limited
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	_, err := c.AddTorrent(context.Background(), srv.URL+"/download/123.torrent", "forage")
	if err == nil {
		t.Fatal("expected the 429 fetch to fail the add")
	}
	if !errors.Is(err, ErrIndexerFetch) {
		t.Fatalf("err = %v, want ErrIndexerFetch in the chain", err)
	}
	if !errors.Is(err, clienterr.ErrTransient) {
		t.Fatalf("err = %v, want ErrTransient (429) in the chain", err)
	}
}

// The non-torrent-response guard (download cap / lapsed session) is
// indexer-side AND transient: caps are per-indexer and time-bound, so
// the deferred flow's failover can rescue the grab from another indexer
// (and the backoff ladder gives the cap a chance to reset).
func TestAddTorrentNonTorrentBodySentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>download limit reached</html>"))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	_, err := c.AddTorrent(context.Background(), srv.URL+"/download/123.torrent", "forage")
	if err == nil {
		t.Fatal("expected the HTML body to fail the add")
	}
	if !errors.Is(err, ErrIndexerFetch) {
		t.Fatalf("err = %v, want ErrIndexerFetch", err)
	}
	if !errors.Is(err, clienterr.ErrTransient) {
		t.Fatalf("err = %v: a download-cap page must classify transient so the deferred failover can rescue it", err)
	}
}
