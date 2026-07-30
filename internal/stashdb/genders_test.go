package stashdb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// One aliased document per batch, ids passed as VARIABLES. Batching is the
// whole point (378 un-owned performers became 8 requests, not 378), and the
// variables matter because these ids come from the database — a query
// assembled by string concatenation is a query someone else can shape.
func TestPerformerGendersBatchesAndUsesVariables(t *testing.T) {
	var requests int
	var sawInterpolatedID bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		if strings.Contains(req.Query, "id-7") {
			sawInterpolatedID = true
		}
		out := map[string]any{}
		for alias, v := range req.Variables {
			id, _ := v.(string)
			out[alias] = map[string]any{"id": id, "gender": "FEMALE"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	defer srv.Close()

	c := NewUnpaced(srv.URL, "k")
	ids := make([]string, 0, 120)
	for i := 0; i < 120; i++ {
		ids = append(ids, "id-"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	got, err := c.PerformerGenders(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 120 {
		t.Fatalf("resolved %d, want 120", len(got))
	}
	// 120 ids at 50 per document.
	if requests != 3 {
		t.Fatalf("made %d requests, want 3 batched ones", requests)
	}
	if sawInterpolatedID {
		t.Fatal("an id was interpolated into the query text instead of bound as a variable")
	}
}

// A performer StashDB no longer knows (deleted or merged) comes back null.
// That has to land as a recorded empty answer rather than an absent key, or
// the caller re-asks about them on every pass forever.
func TestPerformerGendersRecordsNullAsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		out := map[string]any{}
		for alias, v := range req.Variables {
			if v == "gone" {
				out[alias] = nil
				continue
			}
			out[alias] = map[string]any{"id": v, "gender": "MALE"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	defer srv.Close()

	c := NewUnpaced(srv.URL, "k")
	got, err := c.PerformerGenders(context.Background(), []string{"gone", "here"})
	if err != nil {
		t.Fatal(err)
	}
	g, ok := got["gone"]
	if !ok {
		t.Fatal("a deleted performer must still get an entry, else we re-ask forever")
	}
	if g != "" {
		t.Fatalf("deleted performer gender = %q, want empty", g)
	}
	if got["here"] != "MALE" {
		t.Fatalf("here = %q, want MALE", got["here"])
	}
}

// Duplicates are collapsed before hitting the network: the same performer
// appears on many scenes, and paying per appearance is the entire cost this
// pass is trying to avoid.
func TestPerformerGendersDeduplicates(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		asked += len(req.Variables)
		out := map[string]any{}
		for alias, v := range req.Variables {
			out[alias] = map[string]any{"id": v, "gender": "FEMALE"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
	}))
	defer srv.Close()

	c := NewUnpaced(srv.URL, "k")
	got, err := c.PerformerGenders(context.Background(),
		[]string{"a", "a", "b", "", "a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Fatalf("asked StashDB about %d performers, want 2 distinct", asked)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want a and b", got)
	}
}
