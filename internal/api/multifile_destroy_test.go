package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// destroyScene drives postDestroyScene the way the router would, with {id}
// in a chi route context.
func destroyScene(s *Server, rec *httptest.ResponseRecorder, id string) {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req := httptest.NewRequest(http.MethodPost, "/scenes/"+id+"/destroy", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	s.postDestroyScene(rec, req)
}

// stashStub answers only the two operations these tests care about: the
// file-count lookup and the destroy. It records every destroy it is asked
// for, which is the assertion that matters — a refused destroy must reach
// Stash zero times.
type stashStub struct {
	mu        sync.Mutex
	fileCount int // files reported by the by-id lookup (SceneFileCount)
	missing   bool
	// matchScene/matchPaths drive the by-path lookup
	// (FindSceneByPathContains). matchPaths is the scene's FULL file list, so
	// len() > 1 is what the purge guard reads as a multi-file scene. Left
	// empty, the by-path lookup reports not-found.
	matchScene string
	matchPaths []string
	destroyed  []string
}

func (f *stashStub) destroys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.destroyed...)
}

func (f *stashStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "ForagerSceneFiles"):
			f.mu.Lock()
			missing, n := f.missing, f.fileCount
			f.mu.Unlock()
			if missing {
				io.WriteString(w, `{"data":{"findScene":null}}`)
				return
			}
			files := make([]map[string]any, 0, n)
			for i := 0; i < n; i++ {
				files = append(files, map[string]any{"path": "/lib/P/f.mp4"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"findScene": map[string]any{"id": "1", "files": files}},
			})
		case strings.Contains(body.Query, "ForagerFindSceneByPath"):
			f.mu.Lock()
			id, paths := f.matchScene, append([]string{}, f.matchPaths...)
			f.mu.Unlock()
			if len(paths) == 0 {
				io.WriteString(w, `{"data":{"findScenes":{"count":0,"scenes":[]}}}`)
				return
			}
			files := make([]map[string]any, 0, len(paths))
			for _, p := range paths {
				files = append(files, map[string]any{"path": p})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"findScenes": map[string]any{"count": 1, "scenes": []map[string]any{{
					"id": id, "title": "T", "date": "", "stash_ids": []any{}, "files": files,
				}}},
			}})
		case strings.Contains(body.Query, "sceneDestroy"):
			f.mu.Lock()
			if in, ok := body.Variables["input"].(map[string]any); ok {
				f.destroyed = append(f.destroyed, fmt.Sprint(in["id"]))
			}
			f.mu.Unlock()
			io.WriteString(w, `{"data":{"sceneDestroy":true}}`)
		default:
			io.WriteString(w, `{"data":{}}`)
		}
	})
}

func stashStubServer(t *testing.T, stub *stashStub) *Server {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	dbh, err := db.Open(filepath.Join(t.TempDir(), "forager.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	pool := clientpool.New()
	pool.Reload(config.Config{StashURL: srv.URL, StashAPIKey: "k"})
	return &Server{
		db:    dbh,
		pool:  pool,
		grabs: grabs.NewRepo(dbh),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestDestroySceneRefusesMultiFile is the guard on the performer page's
// duplicate-cleanup button. sceneDestroy(delete_file) deletes EVERY file on
// the scene, so pressing "remove this copy" on a scene Stash has filed two
// matching-fingerprint files under would delete the copy being kept, plus the
// scene's tags, o-counter and markers. Refuse rather than over-delete.
func TestDestroySceneRefusesMultiFile(t *testing.T) {
	stub := &stashStub{fileCount: 2}
	s := stashStubServer(t, stub)

	rec := httptest.NewRecorder()
	destroyScene(s, rec, "183951")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a 2-file scene (body=%s)", rec.Code, rec.Body.String())
	}
	if got := stub.destroys(); len(got) != 0 {
		t.Fatalf("destroyed %v; a multi-file scene must never reach sceneDestroy", got)
	}
	if !strings.Contains(rec.Body.String(), "2 files") {
		t.Errorf("error should tell the user how many files are attached, got: %s", rec.Body.String())
	}
}

// TestDestroySceneAllowsSingleFile is the control: ordinary one-file dedup
// cleanup must still work, or the guard has just broken the feature.
func TestDestroySceneAllowsSingleFile(t *testing.T) {
	stub := &stashStub{fileCount: 1}
	s := stashStubServer(t, stub)

	rec := httptest.NewRecorder()
	destroyScene(s, rec, "42")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := stub.destroys(); len(got) != 1 || got[0] != "42" {
		t.Fatalf("destroyed %v, want [42]", got)
	}
}

// TestDestroySceneRefusesWhenCountUnknown: an unknown file count is not a
// green light. If the lookup fails we don't know whether the scene holds one
// file or ten, and the destroy is irreversible.
func TestDestroySceneRefusesWhenCountUnknown(t *testing.T) {
	stub := &stashStub{missing: true}
	s := stashStubServer(t, stub)

	rec := httptest.NewRecorder()
	destroyScene(s, rec, "999")

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200; a failed file-count lookup must not permit a destroy")
	}
	if got := stub.destroys(); len(got) != 0 {
		t.Fatalf("destroyed %v with an unknown file count", got)
	}
}

// TestPurgeGrabKeepsMultiFileScene covers the grab purge. DELETE /grabs/{id}
// resolved the scene by basename and destroyed it with delete_file=true; on a
// scene holding two files that took the file ANOTHER grab had placed. The
// sameParentDir guard can't catch it, because it compares parent-directory
// basenames and both copies sit under the same performer folder.
//
// The purge must instead remove only this grab's own file and leave the scene
// (and its other file) alone.
func TestPurgeGrabKeepsMultiFileScene(t *testing.T) {
	// Stash reports scene 183951 holding TWO files, under a Stash-side path
	// whose parent directory basename ("P") matches the placed file's — the
	// realistic shape, and exactly what makes sameParentDir let this through.
	stub := &stashStub{
		matchScene: "183951",
		matchPaths: []string{
			`Z:\Media\P\Bang.Test.1080p.MP4-WRB.mp4`,
			`Z:\Media\P\Bang.Test.1080p.MP4-WRB.1\redownload.mp4`,
		},
	}
	s := stashStubServer(t, stub)
	ctx := context.Background()

	dir := t.TempDir()
	placed := filepath.Join(dir, "P", "Bang.Test.1080p.MP4-WRB.mp4")
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "P", "keep-me.mp4")
	if err := os.WriteFile(other, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Bang.Test.1080p.MP4-WRB", Client: "sabnzbd",
		Status: "confirmed", PlacedPath: placed, Kind: "single", GrabbedAt: 1,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	g, err := s.grabs.Get(ctx, id)
	if err != nil || g == nil {
		t.Fatalf("get: %v", err)
	}

	out := s.purgeGrab(ctx, g)

	if got := stub.destroys(); len(got) != 0 {
		t.Fatalf("destroyed scene(s) %v; a 2-file scene must be left intact", got)
	}
	if _, serr := os.Stat(placed); !os.IsNotExist(serr) {
		t.Errorf("this grab's own file survived the purge (%v)", serr)
	}
	if _, serr := os.Stat(other); serr != nil {
		t.Errorf("the sibling file was removed: %v", serr)
	}
	joined := strings.Join(out.Errors, " ")
	if !strings.Contains(joined, "files") {
		t.Errorf("purge should report why it skipped the scene destroy, got %v", out.Errors)
	}
}

// TestPurgeGrabDestroysSingleFileScene is the control for the guard above: the
// ordinary case (one file on the scene) must still destroy the Stash scene, or
// purging a grab would start leaving orphan scenes behind.
func TestPurgeGrabDestroysSingleFileScene(t *testing.T) {
	stub := &stashStub{
		matchScene: "77",
		matchPaths: []string{`Z:\Media\P\Solo.Test.1080p.mp4`},
	}
	s := stashStubServer(t, stub)
	ctx := context.Background()

	placed := filepath.Join(t.TempDir(), "P", "Solo.Test.1080p.mp4")
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := s.grabs.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Solo.Test.1080p", Client: "sabnzbd",
		Status: "confirmed", PlacedPath: placed, Kind: "single", GrabbedAt: 1,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	g, err := s.grabs.Get(ctx, id)
	if err != nil || g == nil {
		t.Fatalf("get: %v", err)
	}

	s.purgeGrab(ctx, g)

	if got := stub.destroys(); len(got) != 1 || got[0] != "77" {
		t.Fatalf("destroyed %v, want [77] — a single-file scene should still be destroyed", got)
	}
}
