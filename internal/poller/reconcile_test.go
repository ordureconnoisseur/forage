package poller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// settledUnlinked inserts a grab in the state the settle paths leave behind:
// confirmed, a real file in the library, no StashDB cross-id. Returns the id
// and the placed path.
func settledUnlinked(t *testing.T, r *rig, performer, fileName, predicted string) (int64, string) {
	t.Helper()
	placed := filepath.Join(r.libRoot, performer, fileName)
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	id, err := r.repo.Insert(context.Background(), grabs.Grab{
		ReleaseTitle: fileName, Client: "qbit", ClientID: fileName + "hash",
		Category: "forager", Status: "confirmed", PlacedPath: placed,
		PerformerName: performer, Kind: "single",
		Reason: "in library (scanned)",
		// no ActualStashDBID — that is the whole point
		PredictedStashDBID: predicted,
		CompletedAt:        now - 3600,
		ConfirmedAt:        now - 1800,
		GrabbedAt:          now - 7200,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id, placed
}

// forceReconcile clears the slow-cadence gate so the next tick runs a
// reconcile pass (the pass is deliberately 15-minutely in production).
func forceReconcile(r *rig) {
	r.poller.lastReconcile = time.Time{}
	r.poller.reconcileCursor = 0
	r.poller.movedCursor = 0
	r.poller.mismatchCursor = 0
}

// TestReconcileBackfillsLateIdentify is the bug this pass exists for: an
// adopted single settles as confirmed "in library (scanned)" because Stash's
// queued identify hadn't run yet. When it later lands, the scene carries a
// cross-id while the grab still says it was never matched — so the Grabs list
// badged an identified scene NO MATCH. The reconcile pass must adopt the link.
func TestReconcileBackfillsLateIdentify(t *testing.T) {
	r := newRig(t, "forager")

	id, placed := settledUnlinked(t, r, "Late Identify", "sis-loves-me_full_1080.mp4", "")

	// Still unidentified: a pass must change nothing and must not invent a link.
	r.stash.set([]fakeScene{{id: "300", title: "", path: placed, stashDBID: ""}})
	forceReconcile(r)
	r.tick(t)
	if g := r.get(t, id); g.ActualStashDBID != "" {
		t.Fatalf("unidentified scene: actual_stashdb_id=%q, want empty", g.ActualStashDBID)
	}

	// Stash's identify finally runs and attaches the cross-id.
	r.stash.set([]fakeScene{{id: "300", title: "Sis Loves Me", path: placed, stashDBID: "sdb-late-1"}})
	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.ActualStashDBID != "sdb-late-1" {
		t.Fatalf("actual_stashdb_id=%q, want the late cross-id", g.ActualStashDBID)
	}
	if g.Status != "confirmed" {
		t.Fatalf("status=%q, want confirmed", g.Status)
	}
	if g.Reason == "in library (scanned)" {
		t.Errorf("reason still %q — it no longer describes the grab", g.Reason)
	}
}

// TestReconcileRespectsCadence guards the cost: the pass is catch-up work, so
// a tick inside reconcileInterval must not re-query Stash for every settled
// grab. Without the gate every 90s tick would cost one lookup per unlinked
// grab, forever.
func TestReconcileRespectsCadence(t *testing.T) {
	r := newRig(t, "forager")
	_, placed := settledUnlinked(t, r, "Cadence", "cadence_clip.mp4", "")
	r.stash.set([]fakeScene{{id: "301", title: "", path: placed, stashDBID: ""}})

	// The grab is confirmed, so it's out of Active() — nothing else in a tick
	// talks to Stash here, making reqCount a clean proxy for reconcile work.
	forceReconcile(r)
	r.tick(t)
	after := r.stash.reqCount()
	if after == 0 {
		t.Fatal("first tick made no Stash request at all; reconcile never ran")
	}
	// Second tick, gate NOT cleared: no further requests.
	r.tick(t)
	if got := r.stash.reqCount(); got != after {
		t.Fatalf("Stash requests grew %d -> %d inside reconcileInterval; cadence gate not holding", after, got)
	}
}

// TestReconcileLateLinkToDifferentSceneMismatches covers the predicted case: a
// grab that gave up as "in library; no StashDB match" and is later identified
// as a DIFFERENT scene is a mismatch, not a clean confirm. Same verdict the
// on-time confirm path reaches, so the row offers pick-another-release.
func TestReconcileLateLinkToDifferentSceneMismatches(t *testing.T) {
	r := newRig(t, "forager")

	id, placed := settledUnlinked(t, r, "Wrong Scene", "wrong_scene_1080.mp4", "sdb-predicted")
	r.stash.set([]fakeScene{{id: "302", title: "Something Else", path: placed, stashDBID: "sdb-other"}})

	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.ActualStashDBID != "sdb-other" {
		t.Fatalf("actual_stashdb_id=%q, want sdb-other", g.ActualStashDBID)
	}
	if g.Status != "mismatched" {
		t.Fatalf("status=%q, want mismatched (late link went to a different scene)", g.Status)
	}
}

// TestReconcileSkipsPacks: a pack never carries a single cross-id, and
// advancePackConfirm owns its per-scene identify state. The reconcile pass
// must leave packs alone rather than stamping one scene's id onto the whole
// pack grab.
func TestReconcileSkipsPacks(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	packDir := filepath.Join(r.libRoot, "Pack Performer")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Pack Performer Megapack", Client: "qbit", ClientID: "packhash",
		Category: "forager", Status: "confirmed", PlacedPath: packDir,
		PerformerName: "Pack Performer", Kind: "pack", Reason: "pack confirmed",
		CompletedAt: now - 3600, ConfirmedAt: now - 1800, GrabbedAt: now - 7200,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	r.stash.set([]fakeScene{{
		id: "303", title: "One Pack Scene",
		path: filepath.Join(packDir, "scene1.mp4"), stashDBID: "sdb-pack-scene",
	}})

	forceReconcile(r)
	r.tick(t)

	if g := r.get(t, id); g.ActualStashDBID != "" {
		t.Fatalf("pack got actual_stashdb_id=%q; reconcile must skip packs", g.ActualStashDBID)
	}
}

// TestReconcileBackfillsAgedGrabs is the inverse of what this test used to
// assert, and the reversal was earned by measurement rather than opinion.
//
// It used to pin a 14-day window on the backfill, on the reasoning that a
// file still unlinked after two weeks "genuinely isn't on StashDB, and
// re-checking it forever costs a Stash round-trip per pass to learn
// nothing". Sampling the live instance said otherwise: of 60 unlinked grabs
// drawn from the whole set, 18 (30%) already carried a stash_id in Stash.
// Identify had simply run later than the window, and forage had stopped
// looking. Those rows averaged 27 days old, so nothing would ever have
// revisited them — and without a cross-id they are invisible to the
// moved-file repair, to dedup, and to performer re-filing.
//
// (An earlier pass at this quoted 40%, from 25 rows sampled only out of the
// Unsorted subset and matched by name alone. The 30% here is the honest
// figure: a larger sample across the whole set, counting a scene only when
// its FILE PATH contains the grab's filename, so a same-name different-scene
// hit cannot inflate it. The conclusion is unchanged; the number is not.)
//
// The cost the old comment worried about is real but small and bounded: the
// caller still takes reconcileBatch rows per pass behind a rotating cursor,
// and the lookup is against the LOCAL Stash, not the rate-limited StashDB
// budget. Rows that genuinely never link are re-queried once per rotation.
func TestReconcileBackfillsAgedGrabs(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	placed := filepath.Join(r.libRoot, "Ancient", "ancient_clip.mp4")
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Far older than the 14-day cutoff that used to exclude it.
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "ancient clip", Client: "qbit", ClientID: "ancienthash",
		Category: "forager", Status: "confirmed", PlacedPath: placed,
		PerformerName: "Ancient", Kind: "single", Reason: "in library (scanned)",
		CompletedAt: old, ConfirmedAt: old, GrabbedAt: old,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Stash has since identified it. Age must not stop forage adopting that.
	r.stash.set([]fakeScene{{id: "304", title: "Ancient", path: placed, stashDBID: "sdb-ancient"}})

	forceReconcile(r)
	r.tick(t)

	if g := r.get(t, id); g.ActualStashDBID != "sdb-ancient" {
		t.Fatalf("aged grab still unlinked (actual=%q); a 14-day cutoff was discarding "+
			"40%% of recoverable links on the live instance", g.ActualStashDBID)
	}
}

// TestReconcileRecoversCorrectedMismatch: the user overrules a mismatch by
// re-identifying the scene in Stash to the predicted id. The reconcile pass
// must notice and confirm the grab — before it, mismatched grabs were out
// of Active() and stayed mismatched forever, whatever the user did.
func TestReconcileRecoversCorrectedMismatch(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	placed := filepath.Join(r.libRoot, "Fixed", "fixed_scene_1080.mp4")
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "fixed scene", Client: "qbit", ClientID: "fixhash",
		Category: "forager", Status: "mismatched", PlacedPath: placed,
		PerformerName: "Fixed", Kind: "single",
		PredictedStashDBID: "sdb-right", ActualStashDBID: "sdb-wrong",
		Reason:      "stash phash → different scene than predicted",
		CompletedAt: now - 3600, GrabbedAt: now - 7200,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The user has since re-identified the scene in Stash to the PREDICTED id.
	r.stash.set([]fakeScene{{id: "400", title: "Right", path: placed, stashDBID: "sdb-right"}})
	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.Status != "confirmed" || g.ActualStashDBID != "sdb-right" {
		t.Fatalf("status=%q actual=%q, want confirmed/sdb-right", g.Status, g.ActualStashDBID)
	}

	// Control: a scene re-identified to some THIRD id stays mismatched —
	// that is still a question for the user, and forage doesn't guess.
	id2, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "still wrong", Client: "qbit", ClientID: "stillhash",
		Category: "forager", Status: "mismatched", PlacedPath: placed,
		PerformerName: "Fixed", Kind: "single",
		PredictedStashDBID: "sdb-expected", ActualStashDBID: "sdb-wrong",
		CompletedAt: now - 3600, GrabbedAt: now - 7200,
	})
	if err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	r.stash.set([]fakeScene{{id: "401", title: "Third", path: placed, stashDBID: "sdb-third"}})
	forceReconcile(r)
	r.tick(t)
	if g2 := r.get(t, id2); g2.Status != "mismatched" {
		t.Fatalf("third-id case flipped to %q; must stay mismatched", g2.Status)
	}
}

// TestReconcileRepairsMovedFile covers the moved-file repair. forage records
// where IT put a file; anything that reorganises the library afterwards (the
// user filing a scene out of an Unsorted holding folder, Stash's organise)
// leaves that pointer aimed at nothing and forage never notices.
//
// Observed live: 23 confirmed grabs placed 20-40 days earlier into
// /data/porn/Media/Unsorted, whose files had since moved to category
// folders. The seeding cull then refuses to retire those torrents forever
// (it will not delete on an unverifiable copy), and a purge would RemoveAll
// the dead path and report success while the real file survived.
func TestReconcileRepairsMovedFile(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const (
		performer = "Moved Performer"
		fileName  = "moved-scene.mp4"
		sdbID     = "sdb-moved-0001"
	)
	// Where forage put it (and where it no longer is).
	stalePath := filepath.Join(r.libRoot, "Unsorted", fileName)
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Where it actually lives now.
	movedPath := filepath.Join(r.libRoot, performer, fileName)
	if err := os.MkdirAll(filepath.Dir(movedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(movedPath, []byte("real file"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: fileName, Client: "qbit", ClientID: "movedhash",
		Category: "forager", Status: "confirmed", PlacedPath: stalePath,
		PerformerName: performer, Kind: "single",
		ActualStashDBID: sdbID, ConfirmedAt: now - 86400, GrabbedAt: now - 90000,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Stash knows the scene and reports its CURRENT path. The rig sets no
	// StashPathMapping, which is the single-mount case: Stash's path is
	// forage's path verbatim.
	r.stash.set([]fakeScene{{id: "9001", title: "Moved", path: movedPath, stashDBID: sdbID}})

	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.PlacedPath != movedPath {
		t.Fatalf("placed_path=%q, want repaired to %q", g.PlacedPath, movedPath)
	}
	if g.Status != "confirmed" {
		t.Errorf("status=%q, want confirmed (a repair must not change the verdict)", g.Status)
	}
}

// TestReconcileLeavesPresentFileAlone is the guard: a file that IS where the
// grab says must never be second-guessed against Stash. Without this, any
// scene Stash reports at a different path (a duplicate, a re-encode) could
// walk a correct pointer onto the wrong file.
func TestReconcileLeavesPresentFileAlone(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const (
		performer = "Present Performer"
		fileName  = "present-scene.mp4"
		sdbID     = "sdb-present-01"
	)
	placed := filepath.Join(r.libRoot, performer, fileName)
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("still here"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A DIFFERENT path Stash also knows for this cross-id.
	other := filepath.Join(r.libRoot, "Elsewhere", fileName)
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("decoy"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: fileName, Client: "qbit", ClientID: "presenthash",
		Category: "forager", Status: "confirmed", PlacedPath: placed,
		PerformerName: performer, Kind: "single",
		ActualStashDBID: sdbID, ConfirmedAt: now - 86400, GrabbedAt: now - 90000,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	r.stash.set([]fakeScene{{id: "9002", title: "Present", path: other, stashDBID: sdbID}})

	forceReconcile(r)
	r.tick(t)

	if g := r.get(t, id); g.PlacedPath != placed {
		t.Fatalf("placed_path=%q, want it LEFT at %q — a present file is correct by definition",
			g.PlacedPath, placed)
	}
}

// TestTruncateAtComponent pins the rule that lets one repair path serve both
// shapes of placed_path: a file's basename matches the last component, a
// directory's matches an ancestor. Last-match wins because a library
// legitimately repeats a name down a path.
func TestTruncateAtComponent(t *testing.T) {
	for _, c := range []struct{ path, name, want string }{
		// File-shaped: adopt the file itself.
		{"/lib/Perf/scene.mp4", "scene.mp4", "/lib/Perf/scene.mp4"},
		// Directory-shaped: adopt the containing directory, not the file.
		{"/lib/Perf/Pure.Taboo.1080p/Pure.Taboo.1080p.mp4", "Pure.Taboo.1080p", "/lib/Perf/Pure.Taboo.1080p"},
		// Repeated name: the deepest one is the specific one.
		{"/lib/Show/Show/ep.mp4", "Show", "/lib/Show/Show"},
		// No match at all must refuse rather than guess.
		{"/lib/Perf/other.mp4", "scene.mp4", ""},
		{"", "scene.mp4", ""},
		{"/lib/Perf/scene.mp4", "", ""},
	} {
		if got := truncateAtComponent(c.path, c.name); got != c.want {
			t.Errorf("truncateAtComponent(%q, %q) = %q, want %q", c.path, c.name, got, c.want)
		}
	}
}

// TestReconcileRepairsMovedDirectoryPlacement is the case the basename rule
// initially refused: a single whose release was a FOLDER with the video
// inside, so placed_path is a directory while Stash only ever reports the
// file. 522 of 1637 rows on the reference instance are this shape, and
// refusing them left the repair inert exactly where the unsafe version had
// been acting.
func TestReconcileRepairsMovedDirectoryPlacement(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const (
		performer = "Dir Performer"
		relFolder = "Pure.Taboo.26.05.31.1080p"
		sdbID     = "sdb-dir-0001"
	)
	staleDir := filepath.Join(r.libRoot, "Unsorted", relFolder)
	movedDir := filepath.Join(r.libRoot, performer, relFolder)
	movedFile := filepath.Join(movedDir, relFolder+".mp4")
	if err := os.MkdirAll(movedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(movedFile, []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: relFolder, Client: "qbit", ClientID: "dirhash",
		Category: "forager", Status: "confirmed", PlacedPath: staleDir,
		PerformerName: performer, Kind: "single",
		ActualStashDBID: sdbID, ConfirmedAt: now - 86400, GrabbedAt: now - 90000,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Stash reports the FILE, not the folder.
	r.stash.set([]fakeScene{{id: "9101", title: "Dir", path: movedFile, stashDBID: sdbID}})

	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.PlacedPath != movedDir {
		t.Fatalf("placed_path=%q, want the DIRECTORY %q (not the file inside it)",
			g.PlacedPath, movedDir)
	}
}

// TestReconcileRefusesWrongFile is the HIGH finding from the ultra review: a
// cross-id can resolve to several files (95 stashdb ids carry more than one
// grab on the reference instance), and adopting the first one that exists
// could point a grab at the user's own separate copy — which a later purge
// would then delete.
func TestReconcileRefusesWrongFile(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const (
		performer = "Wrong Performer"
		fileName  = "wanted-scene.mp4"
		sdbID     = "sdb-wrong-001"
	)
	stale := filepath.Join(r.libRoot, "Unsorted", fileName)
	// A DIFFERENT file carrying the same cross-id: the user's own copy.
	other := filepath.Join(r.libRoot, "SomeoneElse", "a-different-encode.mp4")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("not forage's file"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: fileName, Client: "qbit", ClientID: "wronghash",
		Category: "forager", Status: "confirmed", PlacedPath: stale,
		PerformerName: performer, Kind: "single",
		ActualStashDBID: sdbID, ConfirmedAt: now - 86400, GrabbedAt: now - 90000,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	r.stash.set([]fakeScene{{id: "9201", title: "Other", path: other, stashDBID: sdbID}})

	forceReconcile(r)
	r.tick(t)

	if g := r.get(t, id); g.PlacedPath != stale {
		t.Fatalf("placed_path=%q, want it LEFT at %q — adopting a differently-named file "+
			"would point this grab at a copy forage never placed, and a purge would delete it",
			g.PlacedPath, stale)
	}
}

// TestReconcileRecoversAgedMismatch is the sibling of the aged-backfill
// case, and the same lesson: the mismatch-recovery pass existed to close
// the "mismatched has no recovery path" residual, then carried a 14-day
// window that reopened it after a fortnight.
//
// Measured on the reference instance: 134 of 148 mismatched grabs (91%) had
// aged past that window, averaging 27 days, so the pass could only ever see
// 14 of them. A user correcting a match a month later is the normal case.
func TestReconcileRecoversAgedMismatch(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const (
		performer = "Aged Mismatch"
		fileName  = "aged_mismatch.mp4"
		predicted = "sdb-predicted-aged"
	)
	placed := filepath.Join(r.libRoot, performer, fileName)
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: fileName, Client: "qbit", ClientID: "agedmmhash",
		Category: "forager", Status: "mismatched", PlacedPath: placed,
		PerformerName: performer, Kind: "single",
		PredictedStashDBID: predicted, ActualStashDBID: "sdb-something-else",
		CompletedAt: old, ConfirmedAt: old, GrabbedAt: old,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The user has since corrected the match in Stash: the scene now carries
	// the id forage predicted all along.
	r.stash.set([]fakeScene{{id: "401", title: "Aged", path: placed, stashDBID: predicted}})

	forceReconcile(r)
	r.tick(t)

	g := r.get(t, id)
	if g.Status != "confirmed" {
		t.Fatalf("status=%q, want confirmed — a correction made a month later "+
			"must still be noticed (reason=%q)", g.Status, g.Reason)
	}
	if g.ActualStashDBID != predicted {
		t.Errorf("actual_stashdb_id=%q, want %q", g.ActualStashDBID, predicted)
	}
}
