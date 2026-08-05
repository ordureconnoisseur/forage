package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole security argument. A handler that fetched "the URL in this
// parameter" would be an open relay: anyone who can reach forage could make
// forage fetch anything reachable FROM forage — the home network, the Docker
// host, a cloud metadata endpoint. Taking an ID and rebuilding the URL against
// a fixed host makes that shape unrepresentable, and this is the check that
// keeps an ID an ID.
func TestValidImageIDRejectsAnythingButAnID(t *testing.T) {
	if !validImageID("79531ea8-c4f5-4ce4-9305-1be7ae783d8f") {
		t.Error("a real StashDB image id must be accepted")
	}
	if !validImageID("79531EA8-C4F5-4CE4-9305-1BE7AE783D8F") {
		t.Error("hex is hex whatever its case")
	}
	for _, bad := range []string{
		"",
		"../../../../etc/passwd",
		"79531ea8-c4f5-4ce4-9305-1be7ae783d8f/../../secret",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"..%2f..%2fetc%2fpasswd",                   // encoded traversal
		"79531ea8_c4f5_4ce4_9305_1be7ae783d8f",     // wrong separators
		"79531ea8-c4f5-4ce4-9305-1be7ae783d8",      // too short
		"79531ea8-c4f5-4ce4-9305-1be7ae783d8ff",    // too long
		"79531ea8-c4f5-4ce4-9305-1be7ae783d8g",     // not hex
		"79531ea8-c4f5-4ce4-9305-1be7ae783d8f\n",   // trailing junk
		"79531ea8-c4f5-4ce4-9305:1be7ae783d8f",     // separator in the wrong place
	} {
		if validImageID(bad) {
			t.Errorf("must reject %q", bad)
		}
	}
}

// The on-disk name must be derived, never taken from input, so no id can ever
// name a path outside the cache directory.
func TestImageCachePathStaysInsideTheCacheDir(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "forage-imgcache")
	c := newImageCache(dir)
	p := c.path("79531ea8-c4f5-4ce4-9305-1be7ae783d8f")
	// Built with filepath so the assertion holds on either separator.
	if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
		t.Errorf("path %q escaped the cache dir %q", p, dir)
	}
	// Two ids must not collide, and the same id must be stable.
	a := c.path("79531ea8-c4f5-4ce4-9305-1be7ae783d8f")
	b := c.path("368efa7e-832a-48cf-be61-49faa90feb59")
	if a == b {
		t.Error("different ids must not share a cache path")
	}
	if a != c.path("79531ea8-c4f5-4ce4-9305-1be7ae783d8f") {
		t.Error("the same id must map to the same path every time")
	}
}
