package sabnzbd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Contract tests against RECORDED responses from real SABnzbd versions
// (testdata/contract/<version>/, captured by the version-matrix recording
// rig — see docs/bulletproof.md Phase 4). To extend the matrix, record a
// new version directory; the tests walk testdata.
//
// What the matrix pinned when first recorded:
//   - nzo_id format changed: 3.x/4.x hand out "SABnzbd_nzo_*", 5.x plain
//     UUIDs — nothing may assume the prefix.
//   - a set_config success echoes the written config section back, with
//     NO {"status": true} envelope — success is the absence of
//     {"status": false}, which is exactly how EnsureCategory reads it.

func contractVersions(t *testing.T) []string {
	t.Helper()
	dirs, err := os.ReadDir(filepath.Join("testdata", "contract"))
	if err != nil {
		t.Fatalf("no contract fixtures: %v", err)
	}
	var out []string
	for _, d := range dirs {
		if d.IsDir() {
			out = append(out, d.Name())
		}
	}
	if len(out) < 3 {
		t.Fatalf("contract matrix expects 3.x/4.x/5.x recordings, have %v", out)
	}
	return out
}

// serveFixtures dispatches on SAB's mode= query param, answering each mode
// with the recorded body for this version.
func serveFixtures(t *testing.T, version string) *httptest.Server {
	t.Helper()
	byMode := map[string]string{
		"version":    "version.json",
		"queue":      "queue.json",
		"history":    "history.json",
		"get_config": "get_config_categories.json",
		"set_config": "set_config_category.json",
		"addurl":     "addurl.json",
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := byMode[r.URL.Query().Get("mode")]
		if !ok {
			t.Errorf("%s: unexpected mode %q", version, r.URL.Query().Get("mode"))
			http.NotFound(w, r)
			return
		}
		raw, err := os.ReadFile(filepath.Join("testdata", "contract", version, name))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(raw)
	}))
}

func TestContractAcrossVersions(t *testing.T) {
	ctx := context.Background()
	for _, version := range contractVersions(t) {
		srv := serveFixtures(t, version)
		c := New(srv.URL, "contract-key")

		if v, err := c.Version(ctx); err != nil || v != version {
			t.Errorf("%s: Version = %q, %v", version, v, err)
		}
		// Empty instance: both listings must decode cleanly to zero items —
		// an envelope change would fail here first.
		if items, err := c.Queue(ctx); err != nil || len(items) != 0 {
			t.Errorf("%s: Queue = %d items, %v", version, len(items), err)
		}
		if items, err := c.History(ctx, 50, ""); err != nil || len(items) != 0 {
			t.Errorf("%s: History = %d items, %v", version, len(items), err)
		}
		// addurl: every version must yield a usable nzo_id; the recorded
		// bodies span both id formats (prefix-style and 5.x UUIDs).
		if id, err := c.AddURL(ctx, "http://indexer/fake.nzb", "forage-contract"); err != nil || id == "" {
			t.Errorf("%s: AddURL = %q, %v", version, id, err)
		}
		// The recorded set_config success (config echo, no status field)
		// must read as success.
		if err := c.EnsureCategory(ctx, "forage-contract", "/tmp/forage-dl"); err != nil {
			t.Errorf("%s: EnsureCategory = %v", version, err)
		}
		// And the category read-back sees what was written.
		cats, err := c.Categories(ctx)
		if err != nil {
			t.Errorf("%s: Categories: %v", version, err)
		} else {
			found := false
			for _, cat := range cats {
				if cat.Name == "forage-contract" && cat.Dir == "/tmp/forage-dl" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: forage-contract not in %v", version, cats)
			}
		}
		srv.Close()
	}
}
