package sabnzbd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

func TestParseTimeLeft(t *testing.T) {
	cases := map[string]int64{
		"":         0,
		"0:00:00":  0,
		"05:30":    330,
		"1:02:03":  3723,
		"12:00:00": 43200,
		// SAB emits D:HH:MM:SS past 24h; the day component is days, not a
		// base-60 digit (folding it as one counted each day as 60 hours).
		"1:02:03:04": 93784,
		"2:00:00:00": 172800,
		"garbage":    0,
		"1:2:3:4:5":  0,
	}
	for in, want := range cases {
		if got := parseTimeLeft(in); got != want {
			t.Errorf("parseTimeLeft(%q) = %d, want %d", in, got, want)
		}
	}
}

// SAB reports API-level failures as HTTP 200 with {"status": false, "error":
// ...}. Read paths must reject that envelope instead of decoding it as an
// empty queue/history (which the poller reads as "download lost").
func TestErrorEnvelopeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status": false, "error": "API Key Incorrect"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "badkey")
	if _, err := c.Queue(context.Background()); err == nil {
		t.Fatal("Queue with error envelope: want error, got nil")
	} else if !errors.Is(err, clienterr.ErrRejected) {
		t.Fatalf("Queue envelope error not ErrRejected: %v", err)
	} else if !strings.Contains(err.Error(), "API Key Incorrect") {
		t.Fatalf("envelope error message lost: %v", err)
	}
	if _, err := c.History(context.Background(), 10, ""); !errors.Is(err, clienterr.ErrRejected) {
		t.Fatalf("History envelope error not ErrRejected: %v", err)
	}
}

// A normal queue body has no top-level status field and must pass untouched;
// command responses with "status": true must also pass.
func TestErrorEnvelopeAllowsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("mode") {
		case "queue":
			w.Write([]byte(`{"queue": {"kbpersec": "100.0", "slots": [{"nzo_id": "SABnzbd_nzo_x", "status": "Downloading"}]}}`))
		case "addurl":
			w.Write([]byte(`{"status": true, "nzo_ids": ["SABnzbd_nzo_y"]}`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	items, err := c.Queue(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("Queue = %v, %v; want 1 item, nil", items, err)
	}
	if id, err := c.AddURL(context.Background(), "http://example/nzb", ""); err != nil || id != "SABnzbd_nzo_y" {
		t.Fatalf("AddURL = %q, %v; want SABnzbd_nzo_y, nil", id, err)
	}
}

// Transport errors embed the request URL (apikey included) in the message;
// the key must be scrubbed while errors.Is classification still works.
func TestRedactKeyOnTransportError(t *testing.T) {
	const key = "s3cretsabkey"
	// Point at a server that is immediately closed to force a connect error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := New(srv.URL, key)
	_, err := c.Queue(context.Background())
	if err == nil {
		t.Fatal("Queue against closed server: want error, got nil")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("API key leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Fatalf("expected redaction marker in error: %v", err)
	}
	if !errors.Is(err, clienterr.ErrTransient) {
		t.Fatalf("transport error lost ErrTransient class: %v", err)
	}
}
