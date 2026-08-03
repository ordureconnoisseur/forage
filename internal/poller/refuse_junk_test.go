package poller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/torrentmeta"
)

// Every rig here runs with adoption ON ("forager"), which is how the daemon
// is actually configured. It matters: with QbitCategory empty, adoptQbitOrphans
// returns immediately, and the first version of these tests passed only
// because of that. The refusal writes a grab that is failed, unplaced and
// still has a live healthy torrent behind it, which is the exact row shape
// grabs.Repo.Recoverable selects, so with adoption off the tests could not
// see the revive loop they were asserting the absence of.

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
// library browsed from Windows over SMB, exactly what the allowlist exists
// to stop.
func TestSingleFileExecutableIsRefused(t *testing.T) {
	r := newRig(t, "forager")
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
		t.Errorf("placed_path = %q, want empty: nothing may enter the library", g.PlacedPath)
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
	r := newRig(t, "forager")
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

// The mechanism behind the churn the test above measures, asserted directly.
//
// A refused grab is failed, unplaced, and its torrent is still sitting in qBit
// perfectly healthy: that is exactly what grabs.Repo.Recoverable looks for, and
// adoptQbitOrphans revives anything it returns at the top of every tick. The
// grab came back as "completed", was re-judged, was refused again, and wrote
// the row twice a tick forever, which also re-armed notifyFailedGrabs' watermark
// and pushed the user the same alert every couple of minutes.
func TestRefusedGrabIsNotRevivedByAdoption(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()
	src := r.stageFile(t, "Setup.exe")
	id := completeAs(t, r, "refusedhash", "Setup.exe", src)

	refused := r.get(t, id)
	if refused.Status != "failed" || !strings.HasPrefix(refused.Reason, grabs.RefusedPrefix) {
		t.Fatalf("precondition: grab is %q/%q, want a refused failure", refused.Status, refused.Reason)
	}
	// The row must LOOK recoverable in every other respect, or this proves
	// nothing: still unplaced, still carrying its client_id, torrent healthy.
	if refused.PlacedPath != "" || refused.ClientID == "" {
		t.Fatalf("precondition: placed_path=%q client_id=%q, want the recoverable shape",
			refused.PlacedPath, refused.ClientID)
	}
	rec, err := r.repo.Recoverable(ctx, "qbit")
	if err != nil {
		t.Fatalf("Recoverable: %v", err)
	}
	if _, ok := rec[refused.ClientID]; ok {
		t.Errorf("Recoverable returned the refused grab; the adopt sweep will revive it every tick")
	}

	if n, _ := r.poller.AdoptNow(ctx); n != 0 {
		t.Errorf("AdoptNow adopted=%d, want 0: the hash is known, nothing new to take", n)
	}
	after := r.get(t, id)
	if after.Status != "failed" {
		t.Fatalf("status = %q after the adopt sweep, want it to stay failed", after.Status)
	}
	if !strings.HasPrefix(after.Reason, grabs.RefusedPrefix) {
		t.Errorf("reason = %q, want the refusal to survive: the revive path overwrites it with \"recovered: ...\"", after.Reason)
	}
	if after.Rev != refused.Rev {
		t.Errorf("row was rewritten (rev %d → %d) by the adopt sweep", refused.Rev, after.Rev)
	}
}

// Two writers stamp RefusedPrefix: this one and the api's pre-download screen,
// which goes through failGrab and clears the retry fields. They must leave the
// same shape. A grab that was deferred once and later completed still carries
// next_retry_at and fail_kind, and next_retry_at is in the grabs JSON, so a
// leftover value renders as "retrying" on a decision that will never be
// retried. (See api.TestTorrentGrabRefusedBeforeDownloading for the twin.)
func TestRefusalClearsTheRetryFields(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()
	src := r.stageFile(t, "Setup.exe")

	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Setup.exe", Client: "qbit", ClientID: "deferredhash",
		Category: "forager", Status: "queued", PerformerName: "Hazel Moore",
		Kind: "single", GrabbedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Insert does not carry these columns, so they go on with an Update: the
	// row has to genuinely hold them or this test proves nothing.
	seed := r.get(t, id)
	seed.NextRetryAt = time.Now().Add(-time.Hour).Unix()
	seed.FailKind = "transient"
	if err := r.repo.Update(ctx, *seed); err != nil {
		t.Fatalf("seed retry fields: %v", err)
	}
	if check := r.get(t, id); check.NextRetryAt == 0 || check.FailKind == "" {
		t.Fatalf("precondition: retry fields did not persist (next_retry_at=%d fail_kind=%q)",
			check.NextRetryAt, check.FailKind)
	}
	r.qbit.set([]qbit.Torrent{{
		Hash: "deferredhash", Name: "Setup.exe", Category: "forager",
		State: "uploading", Progress: 1, ContentPath: src,
	}})
	r.tick(t)

	g := r.get(t, id)
	if g.Status != "failed" {
		t.Fatalf("status = %q, want failed (reason=%q)", g.Status, g.Reason)
	}
	if g.NextRetryAt != 0 || g.FailKind != "" {
		t.Errorf("grab left parked for retry (next_retry_at=%d fail_kind=%q); a refusal is terminal",
			g.NextRetryAt, g.FailKind)
	}
}

// A single .rar is refused, and the reason has to be the true one. The first
// version told the user "the release was not what it claimed to be", which for
// a packed release is simply wrong: it may be exactly what it claimed, and
// forage has no unpacker. The pre-download screen refuses the same release for
// the same stated reason (see qbit.TestAddTorrentRefusesArchivedRelease), so
// in production this is the backstop for magnets, adopted torrents and usenet.
func TestSingleFileArchiveIsRefusedForTheTrueReason(t *testing.T) {
	r := newRig(t, "forager")
	src := r.stageFile(t, "Hazel.Moore.Scene.rar")
	id := completeAs(t, r, "rarhash1", "Hazel.Moore.Scene.rar", src)

	g := r.get(t, id)
	if g.Status != "failed" {
		t.Fatalf("status = %q, want failed (reason=%q)", g.Status, g.Reason)
	}
	if !strings.Contains(g.Reason, "unpacker") {
		t.Errorf("reason = %q, want it to say forage cannot unpack it", g.Reason)
	}
	if strings.Contains(g.Reason, "claimed") {
		t.Errorf("reason = %q still accuses the release of being fake; a .rar usually is not", g.Reason)
	}
	if _, err := os.Stat(filepath.Join(r.libRoot, "Hazel Moore", "Hazel.Moore.Scene.rar")); err == nil {
		t.Error("the archive was hardlinked into the library; placer.Placeable refuses .rar inside a folder and must here too")
	}
}

// The two layers have to agree on what a video is. placer.placeableExts is
// deliberately generous with containers ("refusing one strands a legitimate
// grab"), and when torrentmeta kept a second, narrower list these containers
// were refused before download and would have been placed without complaint
// one layer later.
//
// This package is where both layers meet, so the agreement is asserted here.
// Note which half catches what: the end-to-end placement below passes either
// way (placer always accepted these), and it is torrentmeta.IsVideo that was
// wrong. Without that line this test would have watched the bug go by.
func TestUnusualContainersThePlacerAcceptsAreNotRefused(t *testing.T) {
	for i, name := range []string{
		"Hazel.Moore.Scene.3gp", "Hazel.Moore.Scene.mpv",
		"Hazel.Moore.Scene.m2v", "Hazel.Moore.Scene.m4p",
	} {
		t.Run(name, func(t *testing.T) {
			if !torrentmeta.IsVideo(name) {
				t.Errorf("placer.Placeable(%q)=%v but torrentmeta.IsVideo=false: the pre-download "+
					"screen would refuse a release the library would have placed", name, placer.Placeable(name))
			}
			r := newRig(t, "forager")
			src := r.stageFile(t, name)
			id := completeAs(t, r, fmt.Sprintf("containerhash%d", i), name, src)

			g := r.get(t, id)
			if g.Status != "placed" {
				t.Fatalf("status = %q, want placed (reason=%q)", g.Status, g.Reason)
			}
			if _, err := os.Stat(filepath.Join(r.libRoot, "Hazel Moore", name)); err != nil {
				t.Errorf("%s was not placed: %v", name, err)
			}
		})
	}
}

// An extension-less single file IS refused, deliberately, and this test exists
// to make that a decision rather than an accident: Stash indexes scenes by
// extension, so a file with none could not become one, and placer.Placeable
// refuses it inside a folder already. Direct-from-site rips do land this way,
// so the cost is real and the refusal has to say something a user can act on.
func TestExtensionlessSingleFileIsRefusedWithAReadableReason(t *testing.T) {
	r := newRig(t, "forager")
	src := r.stageFile(t, "Hazel_Moore_scene")
	id := completeAs(t, r, "noexthash", "Hazel_Moore_scene", src)

	g := r.get(t, id)
	if g.Status != "failed" {
		t.Fatalf("status = %q, want failed (reason=%q)", g.Status, g.Reason)
	}
	if !strings.Contains(g.Reason, "no extension") {
		t.Errorf("reason = %q, want it to name the actual problem", g.Reason)
	}
}

// The cautious half: a single-file download of an ordinary video is placed
// exactly as before. A false refusal costs the user a scene they asked for,
// which is worse than a junk file they can delete.
func TestSingleFileVideoIsStillPlaced(t *testing.T) {
	r := newRig(t, "forager")
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
	r := newRig(t, "forager")
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
