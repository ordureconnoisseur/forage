package qbit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Contract tests against RECORDED responses from real qBittorrent
// versions (testdata/contract/<version>/, captured by the version-matrix
// recording rig — see docs/bulletproof.md Phase 4). The fixtures are the
// authority: every assertion here failed or would have failed against at
// least one real version at some point. To extend the matrix, record a
// new version directory; the tests pick it up by walking testdata.
//
// What the matrix pinned when first recorded:
//   - 4.x answers 409 to editCategory when the savePath is UNCHANGED
//     (5.x answers 200) — the idempotent re-save looked like a failure
//     and EnsureCategory needed the read-back verification it now has.
//   - 5.x renamed torrents/resume → torrents/start (404 on the old name),
//     and pausedDL → stoppedDL in torrent states.
//   - completion_on for an incomplete torrent is 0 on 4.x but -1 on 5.x
//     — consumers must treat <=0 as "not complete", never ==0.

const contractInfoHash = "a2c38aa3d230eb61b7a3b7cb93b0391d321550bc"

type contractMeta struct {
	AppVersion string         `json:"appVersion"`
	Status     map[string]int `json:"status"`
}

func loadContract(t *testing.T) map[string]contractMeta {
	t.Helper()
	dirs, err := os.ReadDir(filepath.Join("testdata", "contract"))
	if err != nil {
		t.Fatalf("no contract fixtures: %v", err)
	}
	out := map[string]contractMeta{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", "contract", d.Name(), "meta.json"))
		if err != nil {
			t.Fatalf("read meta for %s: %v", d.Name(), err)
		}
		var m contractMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode meta for %s: %v", d.Name(), err)
		}
		out[d.Name()] = m
	}
	if len(out) < 2 {
		t.Fatalf("contract matrix needs at least a 4.x and a 5.x recording, have %v", out)
	}
	return out
}

func fixture(t *testing.T, version, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "contract", version, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestContractTorrentInfoDecodes replays each version's recorded
// torrents/info through ListTorrents and pins the fields the poller and
// seeding cull consume.
func TestContractTorrentInfoDecodes(t *testing.T) {
	for version := range loadContract(t) {
		info := fixture(t, version, "info.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v2/torrents/info" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(info)
		}))
		c := New(srv.URL, "", "")
		ts, err := c.ListTorrents(context.Background(), ListOpts{Filter: "all"})
		srv.Close()
		if err != nil {
			t.Errorf("%s: ListTorrents: %v", version, err)
			continue
		}
		if len(ts) != 1 {
			t.Errorf("%s: got %d torrents, want 1", version, len(ts))
			continue
		}
		tor := ts[0]
		if tor.Hash != contractInfoHash {
			t.Errorf("%s: hash = %q", version, tor.Hash)
		}
		if tor.Category != "forage-contract" {
			t.Errorf("%s: category = %q", version, tor.Category)
		}
		if tor.AddedOn <= 0 {
			t.Errorf("%s: added_on = %d, want set", version, tor.AddedOn)
		}
		if tor.ContentPath == "" {
			t.Errorf("%s: content_path empty", version)
		}
		// The freshly-added stopped torrent: 4.x calls it pausedDL, 5.x
		// stoppedDL. Both must be in the set the poller classifies as
		// download-side (see classifyQbitState) — a third spelling means a
		// new qBit renamed states again and the classifier needs teaching.
		if tor.State != "pausedDL" && tor.State != "stoppedDL" {
			t.Errorf("%s: state = %q, not a known stopped-download spelling", version, tor.State)
		}
		// completion_on: 0 on 4.x, -1 on 5.x for an incomplete torrent.
		if tor.CompletionOn > 0 {
			t.Errorf("%s: completion_on = %d for an incomplete torrent", version, tor.CompletionOn)
		}
		if tor.Progress != 0 || tor.Ratio != 0 || tor.SeedingTime != 0 {
			t.Errorf("%s: fresh torrent has progress=%v ratio=%v seeding=%v", version, tor.Progress, tor.Ratio, tor.SeedingTime)
		}
	}
}

// contractServer simulates one recorded version: category endpoints
// answer with the version's recorded status codes, torrents/categories
// serves the recorded body.
func contractServer(t *testing.T, version string, meta contractMeta, existingPath string) *httptest.Server {
	cats := fixture(t, version, "categories.json")
	status := func(key string) int {
		s, ok := meta.Status[key]
		if !ok {
			t.Fatalf("%s: meta.json missing status %q", version, key)
		}
		return s
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/torrents/createCategory":
			w.WriteHeader(status("create_duplicate_same_path"))
		case "/api/v2/torrents/editCategory":
			_ = r.ParseForm()
			if r.Form.Get("savePath") == existingPath {
				w.WriteHeader(status("edit_unchanged"))
			} else {
				w.WriteHeader(status("edit_changed"))
			}
		case "/api/v2/torrents/categories":
			_, _ = w.Write(cats)
		case "/api/v2/torrents/start":
			w.WriteHeader(status("start"))
		case "/api/v2/torrents/resume":
			w.WriteHeader(status("resume"))
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestContractEnsureCategoryResave pins the idempotent re-save on every
// recorded version: the category exists and already points at the wanted
// path. On 4.x both createCategory AND the unchanged edit answer 409 —
// EnsureCategory must read the actual state back and succeed anyway.
func TestContractEnsureCategoryResave(t *testing.T) {
	// The recorded categories.json has forage-contract at /tmp/forage-dl2.
	const existingPath = "/tmp/forage-dl2"
	for version, meta := range loadContract(t) {
		srv := contractServer(t, version, meta, existingPath)
		c := New(srv.URL, "", "")
		err := c.EnsureCategory(context.Background(), "forage-contract", existingPath)
		srv.Close()
		if err != nil {
			t.Errorf("%s: idempotent EnsureCategory = %v, want nil", version, err)
		}
	}
}

// TestContractEnsureCategoryMispointed pins the failure side: the
// category exists but points elsewhere, and the edit is also refused
// (as 4.x does when its stars align badly). EnsureCategory must NOT
// report success off the read-back — the state is genuinely wrong.
func TestContractEnsureCategoryMispointed(t *testing.T) {
	for version, meta := range loadContract(t) {
		if meta.Status["edit_changed"] == 200 {
			// On this version the changed-path edit succeeds, so the
			// mispointed case self-heals — nothing to pin here.
			continue
		}
		srv := contractServer(t, version, meta, "/somewhere/else")
		c := New(srv.URL, "", "")
		err := c.EnsureCategory(context.Background(), "forage-contract", "/somewhere/else")
		srv.Close()
		if err == nil {
			t.Errorf("%s: EnsureCategory reported success with the category mispointed", version)
		}
	}
}

// TestContractResumeRename pins Resume() across the 5.x endpoint rename:
// start-then-resume fallback must succeed against both recorded matrices
// (4.x: start 404 / resume 200; 5.x: start 200 / resume 404).
func TestContractResumeRename(t *testing.T) {
	for version, meta := range loadContract(t) {
		srv := contractServer(t, version, meta, "/tmp/forage-dl2")
		c := New(srv.URL, "", "")
		err := c.Resume(context.Background(), contractInfoHash)
		srv.Close()
		if err != nil {
			t.Errorf("%s: Resume = %v, want nil (start=%d resume=%d)",
				version, err, meta.Status["start"], meta.Status["resume"])
		}
	}
}
