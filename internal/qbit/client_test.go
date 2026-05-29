package qbit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

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
