package stash

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAddScenePerformer pins the additive contract: the helper must send a
// bulkSceneUpdate with performer_ids mode ADD over the EXPLICIT scene-id list
// (so existing performers survive and nothing path-based can mass-mistag), and
// return the count Stash reports. Empty inputs are guarded without a request.
func TestAddScenePerformer(t *testing.T) {
	var gotVars map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		gotVars = req.Variables
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"bulkSceneUpdate":[{"id":"10"},{"id":"11"}]}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	n, err := c.AddScenePerformer(context.Background(), []string{"10", "11"}, "p7")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("applied = %d, want 2", n)
	}
	input, _ := gotVars["input"].(map[string]any)
	if input == nil {
		t.Fatal("no input variable sent")
	}
	if ids, _ := input["ids"].([]any); len(ids) != 2 || ids[0] != "10" || ids[1] != "11" {
		t.Errorf("ids = %v, want [10 11]", input["ids"])
	}
	pi, _ := input["performer_ids"].(map[string]any)
	if pi == nil || pi["mode"] != "ADD" {
		t.Errorf("performer_ids = %v, want mode ADD (additive)", input["performer_ids"])
	}
	if pids, _ := pi["ids"].([]any); len(pids) != 1 || pids[0] != "p7" {
		t.Errorf("performer_ids.ids = %v, want [p7]", pi)
	}

	// Empty id list: no-op, no request.
	gotVars = nil
	if n, err := c.AddScenePerformer(context.Background(), nil, "p7"); err != nil || n != 0 {
		t.Errorf("empty list: n=%d err=%v, want 0/nil", n, err)
	}
	if gotVars != nil {
		t.Error("empty id list must not hit the server")
	}

	// Empty performer id errors (and never hits the server).
	if _, err := c.AddScenePerformer(context.Background(), []string{"1"}, ""); err == nil {
		t.Error("empty performer id should error")
	}
}
