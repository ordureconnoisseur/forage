package stashdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestQueryScenesSinceStopsAtWatermark proves the delta pager: sorted
// UPDATED_AT DESC, it returns scenes at/after the watermark and stops at the
// first older one; since=0 is a full fetch; hardCap bounds the total.
func TestQueryScenesSinceStopsAtWatermark(t *testing.T) {
	// Newest-updated first (the real server sorts DESC; the fake mirrors that).
	page1 := []map[string]any{
		{"id": "s1", "title": "newest", "updated": "2026-06-23T00:00:00Z"},
		{"id": "s2", "title": "mid", "updated": "2026-06-20T00:00:00Z"},
		{"id": "s3", "title": "old", "updated": "2026-06-10T00:00:00Z"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables struct {
				Input struct {
					Page int `json:"page"`
				} `json:"input"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		scenes := []map[string]any{}
		if req.Variables.Input.Page <= 1 {
			scenes = page1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"queryScenes": map[string]any{"count": len(scenes), "scenes": scenes}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	ctx := context.Background()
	since := mustUnix(t, "2026-06-15T00:00:00Z")

	got, err := c.QueryScenesSince(ctx, SceneQuery{PerformerIDs: []string{"p"}}, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "s1" || got[1].ID != "s2" {
		t.Fatalf("delta since watermark = %v, want [s1 s2]", ids(got))
	}

	all, err := c.QueryScenesSince(ctx, SceneQuery{PerformerIDs: []string{"p"}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("full fetch (since=0) = %d, want 3", len(all))
	}

	if capped, _ := c.QueryScenesSince(ctx, SceneQuery{}, 0, 1); len(capped) != 1 {
		t.Errorf("hardCap=1 → %d, want 1", len(capped))
	}
}

func mustUnix(t *testing.T, s string) int64 {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tt.Unix()
}

func ids(ss []Scene) []string {
	o := make([]string, 0, len(ss))
	for _, s := range ss {
		o = append(o, s.ID)
	}
	return o
}
