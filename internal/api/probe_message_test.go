package api

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/clienterr"
)

// TestProbeFailMessages pins that each failure class produces advice a person
// can act on, and that the raw error is kept rather than thrown away.
//
// The classes matter because the FIX differs: an unreachable address is a
// typo'd port or a stopped service, a refusal is a wrong API key, and a 404 is
// the right host with the wrong path. A single generic "probe failed" makes
// the user guess between them.
func TestProbeFailMessages(t *testing.T) {
	refused := &net.OpError{
		Op: "dial", Net: "tcp",
		Err: errors.New("connectex: No connection could be made because the target machine actively refused it"),
	}

	for _, tc := range []struct {
		name    string
		err     error
		wants   []string // all must appear in the message
		notWant string   // must NOT leak into the friendly line
	}{
		{
			name:    "connection refused",
			err:     refused,
			wants:   []string{"Couldn't reach Prowlarr", "http://host:9696", "running", "port"},
			notWant: "connectex",
		},
		{
			name:  "dns failure",
			err:   &net.DNSError{Err: "no such host", Name: "nope.local"},
			wants: []string{"Couldn't reach Prowlarr"},
		},
		{
			name:  "timeout",
			err:   context.DeadlineExceeded,
			wants: []string{"didn't answer", "Prowlarr"},
		},
		{
			name:  "bad api key",
			err:   clienterr.Status("prowlarr", 401, []byte("unauthorized")),
			wants: []string{"refused the request", "API key"},
		},
		{
			name:  "wrong path",
			err:   clienterr.Status("prowlarr", 404, []byte("not found")),
			wants: []string{"isn't Prowlarr's API", "port"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeFail("Prowlarr", "http://host:9696", tc.err)
			if got.OK {
				t.Fatal("probeFail returned OK")
			}
			for _, want := range tc.wants {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q missing %q", got.Message, want)
				}
			}
			if tc.notWant != "" && strings.Contains(got.Message, tc.notWant) {
				t.Errorf("raw error leaked into the friendly message: %q", got.Message)
			}
			if got.Detail == "" {
				t.Error("Detail is empty — the raw error must be kept for diagnosis")
			}
			if !strings.Contains(got.Detail, tc.err.Error()) {
				t.Errorf("Detail %q doesn't carry the original error", got.Detail)
			}
		})
	}
}

// TestProbeFailUnknownErrorStillUsable: an unclassified error must still give
// the user the service and address rather than falling through to something
// empty.
func TestProbeFailUnknownErrorStillUsable(t *testing.T) {
	got := probeFail("qBittorrent", "http://host:8080", errors.New("something odd"))
	if !strings.Contains(got.Message, "qBittorrent") || !strings.Contains(got.Message, "http://host:8080") {
		t.Errorf("message = %q, want the service and address named", got.Message)
	}
	if got.Detail != "something odd" {
		t.Errorf("Detail = %q, want the original error", got.Detail)
	}
}

// TestStashDBTargetDefaults: the StashDB URL is optional (empty means the
// public endpoint), and "Couldn't reach StashDB at ." reads like a bug.
func TestStashDBTargetDefaults(t *testing.T) {
	if got := stashdbTarget(""); got != "stashdb.org" {
		t.Errorf("stashdbTarget(\"\") = %q, want stashdb.org", got)
	}
	if got := stashdbTarget("https://example.org/gql"); got != "https://example.org/gql" {
		t.Errorf("stashdbTarget passed through wrong: %q", got)
	}
}
