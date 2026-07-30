package stashdb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fingerprintServer answers a fingerprint lookup by matching on hash: any
// scene whose batch contains a hash in `known` resolves to that scene id.
func fingerprintServer(t *testing.T, known map[string]string, sent *[][]Fingerprint) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				Fingerprints [][]Fingerprint `json:"fingerprints"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		if sent != nil {
			*sent = append(*sent, req.Variables.Fingerprints...)
		}
		out := make([]([]map[string]string), 0, len(req.Variables.Fingerprints))
		for _, batch := range req.Variables.Fingerprints {
			var hits []map[string]string
			for _, f := range batch {
				if id, ok := known[f.Hash]; ok {
					hits = append(hits, map[string]string{"id": id})
					break
				}
			}
			out = append(out, hits)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"findScenesBySceneFingerprints": out},
		})
	}))
}

// The result must line up with the input by position. Scenes with no hashes
// are dropped from the request (the input type rejects an empty inner list)
// and must still land back in the right slot, or the ids scatter onto the
// wrong scenes and the feed hides releases the user does not own.
func TestFindScenesByFingerprintsKeepsPositions(t *testing.T) {
	var sent [][]Fingerprint
	srv := fingerprintServer(t, map[string]string{"h-b": "sdb-b", "h-d": "sdb-d"}, &sent)
	defer srv.Close()

	got, err := NewUnpaced(srv.URL, "k").FindScenesByFingerprints(context.Background(), [][]Fingerprint{
		{{Hash: "h-a", Algorithm: "PHASH"}},  // 0: no match
		{{Hash: "h-b", Algorithm: "PHASH"}},  // 1: matches sdb-b
		nil,                                  // 2: JavStash-style, no hashes at all
		{{Hash: "h-d", Algorithm: "OSHASH"}}, // 3: matches sdb-d
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "sdb-b", "", "sdb-d"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// The hash-less scene must not have been sent: an empty inner list fails
	// the whole request, taking the three answerable scenes down with it.
	if len(sent) != 3 {
		t.Errorf("sent %d batches, want 3 (the empty one must be filtered out)", len(sent))
	}
}

// Nothing to ask about means no request at all, not an empty query.
func TestFindScenesByFingerprintsNoHashes(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
	}))
	defer srv.Close()

	got, err := NewUnpaced(srv.URL, "k").FindScenesByFingerprints(context.Background(),
		[][]Fingerprint{nil, {}, nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "" || got[1] != "" || got[2] != "" {
		t.Errorf("got %v, want three empties", got)
	}
	if calls != 0 {
		t.Errorf("made %d requests for scenes with no hashes, want 0", calls)
	}
}

// A page bigger than one batch still comes back aligned.
func TestFindScenesByFingerprintsBatches(t *testing.T) {
	known := map[string]string{}
	in := make([][]Fingerprint, 0, 100)
	want := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		h := "h" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		in = append(in, []Fingerprint{{Hash: h, Algorithm: "PHASH"}})
		if i%3 == 0 {
			known[h] = "sdb-" + h
			want = append(want, "sdb-"+h)
		} else {
			want = append(want, "")
		}
	}
	srv := fingerprintServer(t, known, nil)
	defer srv.Close()

	got, err := NewUnpaced(srv.URL, "k").FindScenesByFingerprints(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A short reply must not shift ids onto later scenes. Getting this wrong
// hides scenes the user does not own, which is invisible from the UI.
func TestFindScenesByFingerprintsShortReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"findScenesBySceneFingerprints": [][]map[string]string{{{"id": "sdb-first"}}},
		}})
	}))
	defer srv.Close()

	got, err := NewUnpaced(srv.URL, "k").FindScenesByFingerprints(context.Background(), [][]Fingerprint{
		{{Hash: "h1", Algorithm: "PHASH"}},
		{{Hash: "h2", Algorithm: "PHASH"}},
		{{Hash: "h3", Algorithm: "PHASH"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "sdb-first" || got[1] != "" || got[2] != "" {
		t.Fatalf("got %v, want only the first position filled", got)
	}
}
