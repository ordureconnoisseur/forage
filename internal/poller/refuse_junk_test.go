package poller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// completeAs inserts a grab and runs one tick with qBit reporting it finished
// at srcPath, which is the moment the placer would otherwise be handed the
// download.
func completeAs(t *testing.T, r *rig, hash, name, srcPath string) int64 {
	t.Helper()
	id, err := r.repo.Insert(context.Background(), grabs.Grab{
		ReleaseTitle: name, Client: "qbit", ClientID: hash,
		Category: "forager", Status: "queued", PerformerName: "Hazel Moore",
		Kind: "single", GrabbedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: name, Category: "forager",
		State: "uploading", Progress: 1, ContentPath: srcPath,
	}})
	r.tick(t)
	return id
}

// A single-file download that is itself an executable must be refused, not
// placed. The placer's allowlist only filters files INSIDE a release folder,
// so without this an .exe served as the whole "scene" is hardlinked into a
// library browsed from Windows over SMB — exactly what the allowlist exists
// to stop.
func TestSingleFileExecutableIsRefused(t *testing.T) {
	r := newRig(t, "")
	src := r.stageFile(t, "Hazel.Moore.Scene.1080p.mp4.exe")

	id := completeAs(t, r, "junkhash1", "Hazel.Moore.Scene.1080p.mp4.exe", src)

	g := r.get(t, id)
	if g.Status != "failed" {
		t.Fatalf("status = %q, want failed (reason=%q)", g.Status, g.Reason)
	}
	if !strings.HasPrefix(g.Reason, grabs.RefusedPrefix) {
		t.Errorf("reason = %q, want the refusal prefix so the user can tell refusal from failure", g.Reason)
	}
	if !strings.Contains(g.Reason, ".exe") {
		t.Errorf("reason = %q, want it to name the file type it refused", g.Reason)
	}
	if g.PlacedPath != "" {
		t.Errorf("placed_path = %q, want empty — nothing may enter the library", g.PlacedPath)
	}
	if _, err := os.Stat(filepath.Join(r.libRoot, "Hazel Moore")); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(filepath.Join(r.libRoot, "Hazel Moore"))
		t.Errorf("library folder was created and holds %v; a refused grab must place nothing", entries)
	}
}

// Refusal must be TERMINAL. Left in "completed", the grab would be re-judged
// (and, before the allowlist, re-placed) on every tick for the daemon's
// lifetime; the whole reason this lives in the grab layer is that only here
// can the answer stick.
func TestRefusedGrabDoesNotRetryNextTick(t *testing.T) {
	r := newRig(t, "")
	src := r.stageFile(t, "install.exe")
	id := completeAs(t, r, "junkhash2", "install.exe", src)

	before := r.get(t, id)
	r.tick(t)
	r.tick(t)
	after := r.get(t, id)
	if after.Status != "failed" {
		t.Fatalf("status = %q after further ticks, want it to stay failed", after.Status)
	}
	if after.Rev != before.Rev {
		t.Errorf("row was rewritten (rev %d → %d): a settled refusal must not churn every tick",
			before.Rev, after.Rev)
	}
}

// The cautious half: a single-file download of an ordinary video is placed
// exactly as before. A false refusal costs the user a scene they asked for,
// which is worse than a junk file they can delete.
func TestSingleFileVideoIsStillPlaced(t *testing.T) {
	r := newRig(t, "")
	src := r.stageFile(t, "Hazel.Moore.Scene.1080p.mp4")
	id := completeAs(t, r, "goodhash1", "Hazel.Moore.Scene.1080p.mp4", src)

	g := r.get(t, id)
	if g.Status != "placed" {
		t.Fatalf("status = %q, want placed (reason=%q)", g.Status, g.Reason)
	}
	if _, err := os.Stat(filepath.Join(r.libRoot, "Hazel Moore", "Hazel.Moore.Scene.1080p.mp4")); err != nil {
		t.Errorf("the video was not placed: %v", err)
	}
}

// A multi-file release is the placer's business, not the grab layer's: it
// filters per file, so a folder carrying a video plus passengers must still
// be placed and only the passengers dropped. Refusing at the folder level
// would throw the scene away with them.
func TestMultiFileReleaseWithJunkIsStillPlaced(t *testing.T) {
	r := newRig(t, "")
	rel := filepath.Join(r.stage, "Hazel.Moore.Release.XXX.1080p")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"scene.mp4", "Setup.exe", "info.nfo"} {
		if err := os.WriteFile(filepath.Join(rel, f), []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	id := completeAs(t, r, "goodhash2", "Hazel.Moore.Release.XXX.1080p", rel)

	g := r.get(t, id)
	if g.Status != "placed" {
		t.Fatalf("status = %q, want placed (reason=%q)", g.Status, g.Reason)
	}
	dest := filepath.Join(r.libRoot, "Hazel Moore", "Hazel.Moore.Release.XXX.1080p")
	if _, err := os.Stat(filepath.Join(dest, "scene.mp4")); err != nil {
		t.Errorf("the video was not placed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Setup.exe")); !os.IsNotExist(err) {
		t.Errorf("Setup.exe reached the library; the placer's allowlist should have dropped it")
	}
}

// refuseUnplaceableDownload is asked about a path that may be missing (a
// dropped NAS mount, a client still moving files in). "Can't see it" must
// never read as "junk", or a perfectly good grab is failed for a mount blip.
func TestRefuseUnplaceableDownloadIsSilentOnUnknownPaths(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, path string
	}{
		{"missing path", filepath.Join(dir, "gone", "Setup.exe")},
		{"directory", dir},
	} {
		if got := refuseUnplaceableDownload(c.path); got != "" {
			t.Errorf("%s: refused with %q, want no opinion", c.name, got)
		}
	}
}
