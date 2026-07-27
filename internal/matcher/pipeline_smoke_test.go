package matcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// Pipeline smoke test: the full Match path — tokenize → entity scan →
// StashDB fan-out (structured + text tracks) → score → rank — against a
// synthetic corpus and a fake StashDB, entirely in CI. This is NOT the
// accuracy bench (that runs against the private corpus on the live
// instance, per CONTRIBUTING); it pins the WIRING: entity ids translate to
// StashDB ids, every track contributes candidates, and canonical release
// shapes rank their scene first. A regression here means the pipeline
// broke, not that a heuristic drifted.

type fakeScene struct {
	id, title, date, studioID, studioName string
	performerIDs                          []string
	performerNames                        []string
}

var fakeWorld = []fakeScene{
	{"sc-challenge", "Slut Challenge", "2024-08-23", "sdb-s1", "Tough Love X", []string{"sdb-p1"}, []string{"Liora Vane"}},
	{"sc-winter", "Winter Rendezvous", "2025-01-10", "sdb-s1", "Tough Love X", []string{"sdb-p1"}, []string{"Liora Vane"}},
	{"sc-backyard", "Backyard Antics", "2023-05-01", "sdb-s2", "Analyzed Media", []string{"sdb-p2"}, []string{"Mira Solen"}},
	{"sc-crossover", "Double Feature", "2024-02-14", "sdb-s2", "Analyzed Media", []string{"sdb-p1", "sdb-p2"}, []string{"Liora Vane", "Mira Solen"}},
}

func (f fakeScene) wire() map[string]any {
	performers := make([]map[string]any, len(f.performerIDs))
	for i := range f.performerIDs {
		performers[i] = map[string]any{
			"performer": map[string]any{"id": f.performerIDs[i], "name": f.performerNames[i]},
			"as":        "",
		}
	}
	return map[string]any{
		"id":         f.id,
		"title":      f.title,
		"date":       f.date,
		"studio":     map[string]any{"id": f.studioID, "name": f.studioName},
		"performers": performers,
		"urls":       []any{},
		"images":     []any{},
		"tags":       []any{},
		"updated":    "2025-01-01T00:00:00Z",
	}
}

// fakeStashDB answers the two GraphQL operations Match uses, filtering
// fakeWorld the way StashDB's semantics say: performers INCLUDES_ALL,
// studios INCLUDES, date EQUALS; text search by word overlap with title +
// performer names.
func fakeStashDB(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad graphql body: %v", err)
			http.Error(w, "bad body", 400)
			return
		}
		var matched []map[string]any
		switch {
		case strings.Contains(body.Query, "queryScenes"):
			input, _ := body.Variables["input"].(map[string]any)
			for _, s := range fakeWorld {
				if sceneMatchesQuery(s, input) {
					matched = append(matched, s.wire())
				}
			}
			writeGQL(w, "queryScenes", matched)
		case strings.Contains(body.Query, "searchScenes"):
			term, _ := body.Variables["term"].(string)
			words := strings.Fields(strings.ToLower(term))
			for _, s := range fakeWorld {
				hay := strings.ToLower(s.title + " " + strings.Join(s.performerNames, " "))
				for _, wd := range words {
					if len(wd) > 2 && strings.Contains(hay, wd) {
						matched = append(matched, s.wire())
						break
					}
				}
			}
			writeGQL(w, "searchScenes", matched)
		default:
			t.Errorf("unexpected graphql operation: %.80s", body.Query)
			http.Error(w, "unknown op", 400)
		}
	}))
}

func sceneMatchesQuery(s fakeScene, input map[string]any) bool {
	if f, ok := input["performers"].(map[string]any); ok {
		vals, _ := f["value"].([]any)
		for _, v := range vals { // INCLUDES_ALL
			if !containsStr(s.performerIDs, v.(string)) {
				return false
			}
		}
	}
	if f, ok := input["studios"].(map[string]any); ok {
		vals, _ := f["value"].([]any)
		any := false
		for _, v := range vals { // INCLUDES
			if s.studioID == v.(string) {
				any = true
			}
		}
		if !any {
			return false
		}
	}
	if f, ok := input["date"].(map[string]any); ok {
		if v, _ := f["value"].(string); v != s.date {
			return false
		}
	}
	return true
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func writeGQL(w http.ResponseWriter, field string, scenes []map[string]any) {
	if scenes == nil {
		scenes = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{
			field: map[string]any{"count": len(scenes), "scenes": scenes},
		},
	})
}

func TestMatchPipelineEndToEnd(t *testing.T) {
	ctx := context.Background()
	dbh, err := db.Open(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()

	// Synthetic corpus: two performers (one with an alias) and two studios,
	// with the cross-ids the scanner must translate through.
	for _, row := range [][4]string{
		{"p1", "sdb-p1", "Liora Vane", `["Liora V"]`},
		{"p2", "sdb-p2", "Mira Solen", `[]`},
	} {
		if _, err := dbh.Exec(`INSERT INTO performer_cache (stash_id, stashdb_id, name, aliases, refreshed_at) VALUES (?,?,?,?,1)`,
			row[0], row[1], row[2], row[3]); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][3]string{
		{"sdb-s1", "Tough Love X", `["ToughLoveX"]`},
		{"sdb-s2", "Analyzed Media", `[]`},
	} {
		if _, err := dbh.Exec(`INSERT INTO studio_cache (stashdb_id, name, aliases, refreshed_at) VALUES (?,?,?,1)`,
			row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}

	srv := fakeStashDB(t)
	defer srv.Close()
	m, err := New(ctx, dbh, stashdb.NewUnpaced(srv.URL, "test-key"))
	if err != nil {
		t.Fatal(err)
	}

	// Canonical release-name shapes, each expecting its scene at rank 1.
	cases := []struct {
		release string
		want    string
	}{
		// Bracketed site + performer + title + dotted date.
		{"[ToughLoveX.com] Liora Vane - Slut Challenge (24.08.23) [1080p]", "sc-challenge"},
		// Scene-style dotted release with studio prefix and ISO-ish date.
		{"Analyzed.Media.23.05.01.Mira.Solen.Backyard.Antics.XXX.1080p.MP4-GRP", "sc-backyard"},
		// Same performer, different scene — the title must decide.
		{"Liora Vane Winter Rendezvous 1080p", "sc-winter"},
		// Two-performer release: INCLUDES_ALL narrows to the crossover.
		{"Liora Vane and Mira Solen - Double Feature (2024)", "sc-crossover"},
	}
	for _, tc := range cases {
		cands, err := m.Match(ctx, tc.release)
		if err != nil {
			t.Errorf("Match(%q): %v", tc.release, err)
			continue
		}
		if len(cands) == 0 {
			t.Errorf("Match(%q): no candidates", tc.release)
			continue
		}
		if cands[0].Scene.ID != tc.want {
			t.Errorf("Match(%q) top = %s (conf %.2f), want %s",
				tc.release, cands[0].Scene.ID, cands[0].Confidence, tc.want)
		}
	}
}
