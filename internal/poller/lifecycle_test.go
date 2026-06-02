package poller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/clientpool"
	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// Happy-path smoke tests that drive a grab through the poller's state
// machine one tickOnce() at a time, with qBit + Stash faked at the HTTP
// layer (Option A from the test brief). The rest of the pipeline is REAL:
// a real grabs.Repo on a temp SQLite db, a real placer hardlinking into a
// temp library dir, a real clientpool.Pool built via Reload() against
// httptest servers. No production code is modified or interface-ified —
// the clients already take a baseURL + use a stdlib http.Client, so
// pointing them at test servers is enough.
//
// Each tick is driven synchronously (call tickOnce, wait for it to
// return, then step the fakes' responses) so there's no reliance on the
// background goroutine/timer and no sleeps that could race.
//
// Intentionally NOT covered here (would belong in a fuller suite): the
// pack confirm/dedup settle logic (advancePackConfirm, dedupPack), the
// SAB client path (advanceSab), qBit info-hash enrichment via
// pickRecent (already unit-tested in grabs_test.go), and the many
// failure/timeout branches (qbitLinkTimeout, sabRegisterGrace, place
// errors). These four tests hit the transitions that matter on the
// single-grab qBit happy path plus the two recent self-heal/adoption
// fixes.

// ── fake qBit ───────────────────────────────────────────────────────

// fakeQbit serves the slice of /api/v2/torrents/info and /files the
// poller reads. The test mutates torrents/files between ticks under the
// lock to step the simulated download forward.
type fakeQbit struct {
	mu       sync.Mutex
	torrents []qbit.Torrent
	files    map[string][]qbit.TorrentFile // info_hash → file list
}

func (f *fakeQbit) set(ts []qbit.Torrent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torrents = ts
}

func (f *fakeQbit) handler() http.Handler {
	mux := http.NewServeMux()
	// Real qBit replies "Ok." to a no-auth probe; the poller never logs
	// in here (username is empty) but keep it harmless.
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Ok.")
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ts := f.torrents
		f.mu.Unlock()
		writeJSON(w, ts)
	})
	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		fl := f.files[r.URL.Query().Get("hash")]
		f.mu.Unlock()
		writeJSON(w, fl)
	})
	return mux
}

// ── fake Stash ──────────────────────────────────────────────────────

// fakeScene is one row fakeStash returns from findScenes. An empty
// stashDBID means "in Stash but not cross-id'd yet" (drives the scanned
// branch); a set one drives confirmed/mismatched.
type fakeScene struct {
	id, title, path, stashDBID string
}

// fakeStash answers the GraphQL calls the poller makes. The test sets
// `scenes` between ticks to control what FindSceneByPathContains returns,
// stepping a grab from placed → (re-scan) → confirmed/scanned.
type fakeStash struct {
	mu     sync.Mutex
	scenes []fakeScene
	reqs   int // total GraphQL requests served (lets a test prove a code path made no calls)
}

func (f *fakeStash) set(scenes []fakeScene) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scenes = scenes
}

func (f *fakeStash) reqCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func (f *fakeStash) handler() http.Handler {
	const stashDBEndpoint = "https://stashdb.org/graphql"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs++
		f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		q := string(body)
		switch {
		case strings.Contains(q, "metadataIdentify"):
			writeRaw(w, `{"data":{"metadataIdentify":"identify-job-1"}}`)
		case strings.Contains(q, "metadataScan"):
			writeRaw(w, `{"data":{"metadataScan":"scan-job-1"}}`)
		case strings.Contains(q, "stashBoxes"):
			writeRaw(w, `{"data":{"configuration":{"general":{"stashBoxes":[{"endpoint":"`+stashDBEndpoint+`"}]}}}}`)
		case strings.Contains(q, "findScenes"):
			f.mu.Lock()
			scenes := f.scenes
			f.mu.Unlock()
			writeJSON(w, findScenesResponse(scenes, stashDBEndpoint))
		default:
			// Any other query the poller might issue resolves to an empty
			// data object rather than a GraphQL error.
			writeRaw(w, `{"data":{}}`)
		}
	})
}

// findScenesResponse hand-builds the GraphQL envelope the stash client's
// FindSceneByPathContains decodes (data.findScenes.{count,scenes[]}).
func findScenesResponse(scenes []fakeScene, endpoint string) map[string]any {
	out := make([]map[string]any, 0, len(scenes))
	for _, s := range scenes {
		var stashIDs []map[string]any
		if s.stashDBID != "" {
			stashIDs = []map[string]any{{"endpoint": endpoint, "stash_id": s.stashDBID}}
		}
		out = append(out, map[string]any{
			"id":        s.id,
			"title":     s.title,
			"date":      "",
			"stash_ids": stashIDs,
			"files":     []map[string]any{{"path": s.path}},
		})
	}
	return map[string]any{
		"data": map[string]any{
			"findScenes": map[string]any{"count": len(out), "scenes": out},
		},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, s)
}

// ── rig ─────────────────────────────────────────────────────────────

type rig struct {
	poller  *Poller
	repo    *grabs.Repo
	qbit    *fakeQbit
	stash   *fakeStash
	libRoot string // <tmp>/library
	stage   string // <tmp>/staging (the download client's "complete" dir)
}

// newRig stands up the full real pipeline against fake qBit/Stash HTTP
// servers. qbitCategory enables (non-empty) or disables (empty) orphan
// adoption.
func newRig(t *testing.T, qbitCategory string) *rig {
	t.Helper()

	dbh, err := db.Open(filepath.Join(t.TempDir(), "forager.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { dbh.Close() })
	repo := grabs.NewRepo(dbh)

	fq := &fakeQbit{files: map[string][]qbit.TorrentFile{}}
	fs := &fakeStash{}
	qSrv := httptest.NewServer(fq.handler())
	sSrv := httptest.NewServer(fs.handler())
	t.Cleanup(qSrv.Close)
	t.Cleanup(sSrv.Close)

	base := t.TempDir()
	libRoot := filepath.Join(base, "library")
	stage := filepath.Join(base, "staging")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatalf("mkdir stage: %v", err)
	}

	pool := clientpool.New()
	pool.Reload(config.Config{
		StashURL:     sSrv.URL,
		StashAPIKey:  "test-key",
		QbitURL:      qSrv.URL,
		QbitCategory: qbitCategory, // empty disables adoptOrphans
		LibraryRoot:  libRoot,
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(repo, dbh, pool, log, time.Minute, 6*time.Hour)

	return &rig{poller: p, repo: repo, qbit: fq, stash: fs, libRoot: libRoot, stage: stage}
}

// stageFile writes a real file into the staging dir and returns its path,
// to stand in for the download client's completed-download location that
// the placer hardlinks out of.
func (r *rig) stageFile(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(r.stage, name)
	if err := os.WriteFile(p, []byte("fake media bytes"), 0o644); err != nil {
		t.Fatalf("stage file: %v", err)
	}
	return p
}

func (r *rig) tick(t *testing.T) {
	t.Helper()
	if err := r.poller.tickOnce(context.Background()); err != nil {
		t.Fatalf("tickOnce: %v", err)
	}
}

func (r *rig) get(t *testing.T, id int64) *grabs.Grab {
	t.Helper()
	g, err := r.repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("repo.Get(%d): %v", id, err)
	}
	if g == nil {
		t.Fatalf("grab %d not found", id)
	}
	return g
}

// ── tests ───────────────────────────────────────────────────────────

// TestLifecycleSingleGrabConfirmed drives one qBit grab through the full
// happy path: queued → downloading → placed → confirmed, asserting each
// transition and that the file is hardlinked into the library under the
// performer folder, ending with the predicted StashDB id matching.
func TestLifecycleSingleGrabConfirmed(t *testing.T) {
	r := newRig(t, "") // adoption off
	ctx := context.Background()

	const (
		hash      = "abc123hash"
		predicted = "scene-stashdb-id-001"
		performer = "Hazel Moore"
		fileName  = "Hazel.Moore.Release.2160p.mkv"
	)
	src := r.stageFile(t, fileName)

	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle:       "Hazel Moore - Release",
		Client:             "qbit",
		ClientID:           hash, // pre-linked: skips info-hash enrichment
		Category:           "forager",
		Status:             "queued",
		PredictedStashDBID: predicted,
		PerformerName:      performer,
		Kind:               "single",
		GrabbedAt:          time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Tick 1: qBit reports the torrent downloading (progress 0.5).
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: fileName, Category: "forager",
		State: "downloading", Progress: 0.5, ContentPath: src,
	}})
	r.tick(t)
	if g := r.get(t, id); g.Status != "downloading" {
		t.Fatalf("after tick 1: status=%q, want downloading", g.Status)
	}

	// Tick 2: download complete (progress 1, seeding) but Stash hasn't
	// indexed the file yet → poller places it, status stays placed.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: fileName, Category: "forager",
		State: "uploading", Progress: 1, ContentPath: src,
	}})
	r.tick(t)
	g := r.get(t, id)
	if g.Status != "placed" {
		t.Fatalf("after tick 2: status=%q, want placed (reason=%q)", g.Status, g.Reason)
	}
	wantPath := filepath.Join(r.libRoot, performer, fileName)
	if g.PlacedPath != wantPath {
		t.Fatalf("placed_path=%q, want %q", g.PlacedPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("placed file not present in library: %v", err)
	}
	// It must be a hardlink (same inode) of the staged source, not a copy.
	if !sameFile(t, src, wantPath) {
		t.Fatalf("placed file is not hardlinked to the source")
	}

	// Tick 3: Stash has now indexed the file with the predicted StashDB
	// cross-id → confirmed.
	r.stash.set([]fakeScene{{id: "1", title: "Hazel Moore", path: wantPath, stashDBID: predicted}})
	r.tick(t)
	g = r.get(t, id)
	if g.Status != "confirmed" {
		t.Fatalf("after tick 3: status=%q, want confirmed (reason=%q)", g.Status, g.Reason)
	}
	if g.ActualStashDBID != predicted {
		t.Fatalf("actual_stashdb_id=%q, want %q", g.ActualStashDBID, predicted)
	}
}

// TestLifecycleMismatch confirms the terminal mismatched status when
// Stash cross-ids the placed file to a DIFFERENT scene than predicted.
func TestLifecycleMismatch(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()

	const (
		hash      = "def456hash"
		predicted = "predicted-id"
		actual    = "totally-different-id"
		performer = "Angela White"
		fileName  = "release.mkv"
	)
	src := r.stageFile(t, fileName)

	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Angela White - Release", Client: "qbit", ClientID: hash,
		Category: "forager", Status: "queued", PredictedStashDBID: predicted,
		PerformerName: performer, Kind: "single", GrabbedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Single tick that completes + places (Stash already has the file,
	// cross-id'd to the wrong scene) → mismatched.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: fileName, Category: "forager",
		State: "uploading", Progress: 1, ContentPath: src,
	}})
	r.stash.set([]fakeScene{{id: "9", title: "Some Other Scene",
		path: filepath.Join(r.libRoot, performer, fileName), stashDBID: actual}})
	r.tick(t)

	g := r.get(t, id)
	if g.Status != "mismatched" {
		t.Fatalf("status=%q, want mismatched (reason=%q)", g.Status, g.Reason)
	}
	if g.ActualStashDBID != actual {
		t.Fatalf("actual_stashdb_id=%q, want %q", g.ActualStashDBID, actual)
	}
}

// TestLifecycleOrphanAdoption verifies a qBit torrent under the forager
// category that forage isn't tracking gets a grab row created by
// adoptOrphans, keyed on the torrent hash. This is the path the user adds
// torrents through manually.
func TestLifecycleOrphanAdoption(t *testing.T) {
	r := newRig(t, "forager") // adoption ON
	ctx := context.Background()

	const hash = "orphan789hash"
	// AddedOn must be older than adoptionGrace (5m) so it's eligible this
	// tick rather than being held for its own grab to claim it.
	addedOn := time.Now().Add(-10 * time.Minute).Unix()
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: "Manually Added.mkv", Category: "forager",
		State: "downloading", Progress: 0.3, AddedOn: addedOn,
	}})
	// classifyTorrent reads the file list; a single video → "single".
	r.qbit.files[hash] = []qbit.TorrentFile{{Name: "Manually Added.mkv", Size: 1}}

	// No grab exists yet.
	known, err := r.repo.KnownClientIDs(ctx)
	if err != nil {
		t.Fatalf("known: %v", err)
	}
	if known[hash] {
		t.Fatalf("hash already known before adoption")
	}

	r.tick(t)

	active, err := r.repo.Active(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	var adopted *grabs.Grab
	for i := range active {
		if active[i].ClientID == hash {
			adopted = &active[i]
			break
		}
	}
	if adopted == nil {
		t.Fatalf("no grab row adopted for hash %q (active=%d)", hash, len(active))
	}
	if adopted.Client != "qbit" {
		t.Fatalf("adopted grab client=%q, want qbit", adopted.Client)
	}
	// NB: the adopted grab is advanced in this same tick (it flows through
	// the normal pipeline immediately), so its `reason` is already the
	// downloading-state reason rather than "adopted from qbit" — the row's
	// existence keyed on the torrent hash is the adoption assertion.
}

// TestLifecyclePrematurePlacementHeal guards the recent self-heal fix: a
// grab carrying a placed_path while qBit still reports the torrent
// incomplete (progress < 1) must have the premature library copy removed
// and be reset to downloading so placement re-runs cleanly on real
// completion.
func TestLifecyclePrematurePlacementHeal(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()

	const (
		hash      = "heal000hash"
		performer = "Riley Reid"
		fileName  = "partial.mkv"
	)
	// Create the bogus premature placement physically so we can assert it
	// gets removed.
	placedDir := filepath.Join(r.libRoot, performer)
	if err := os.MkdirAll(placedDir, 0o755); err != nil {
		t.Fatalf("mkdir placed: %v", err)
	}
	placedPath := filepath.Join(placedDir, fileName)
	if err := os.WriteFile(placedPath, []byte("half a file"), 0o644); err != nil {
		t.Fatalf("write placed: %v", err)
	}

	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Riley Reid - Release", Client: "qbit", ClientID: hash,
		Category: "forager", Status: "placed", PlacedPath: placedPath,
		PlacedAt: time.Now().Unix(), PerformerName: performer, Kind: "single",
		Progress: 1, GrabbedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// qBit reports the torrent still downloading (progress < 1) — the
	// authority that contradicts the placed_path.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: fileName, Category: "forager",
		State: "downloading", Progress: 0.4,
	}})
	r.tick(t)

	g := r.get(t, id)
	if g.Status != "downloading" {
		t.Fatalf("status=%q, want downloading (reason=%q)", g.Status, g.Reason)
	}
	if g.PlacedPath != "" {
		t.Fatalf("placed_path=%q, want cleared", g.PlacedPath)
	}
	if _, err := os.Stat(placedPath); !os.IsNotExist(err) {
		t.Fatalf("premature placement not removed (stat err=%v)", err)
	}
}

// sameFile reports whether two paths are hardlinks of the same inode.
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(ai, bi)
}

// TestPartialDownloadNotCompleted pins that progress — not the qBit state
// name — governs completion. A torrent stopped mid-download (qBit v5
// "stoppedDL") at 80% must stay "downloading" and NOT be placed/scanned. The
// old classifyQbitState default mapped unrecognized states to "completed",
// so a stoppedDL/transient state triggered a premature placement + Stash scan
// of a half-downloaded pack (the Sara Diamante symptom).
func TestPartialDownloadNotCompleted(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()
	const (
		hash     = "partialhash"
		fileName = "Sara.Diamante.Pack.2160p.mkv"
	)
	src := r.stageFile(t, fileName)
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Sara Diamante - Pack",
		Client:       "qbit",
		ClientID:     hash,
		Category:     "forager",
		Status:       "downloading",
		Kind:         "pack",
		GrabbedAt:    time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Stopped mid-download at 80% — the v5 state name the old default would
	// have read as completed.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: fileName, Category: "forager",
		State: "stoppedDL", Progress: 0.8, ContentPath: src,
	}})
	r.tick(t)
	g := r.get(t, id)
	if g.Status != "downloading" {
		t.Fatalf("partial (0.8) stoppedDL: status=%q, want downloading", g.Status)
	}
	if g.PlacedPath != "" {
		t.Fatalf("partial download must not be placed, got placed_path=%q", g.PlacedPath)
	}
}

// TestDedupPackGateBlocksUnverified pins the data-loss safety property:
// the irreversible keep="pack" branch (which deletes copies that were
// ALREADY in the library) must not run when the pack scan isn't verifiably
// complete. Here PackFiles==0 — the magnet/manual case where
// packScanCoverageOK is vacuously true — so coverageVerified is false. The
// gate returns before any Stash lookup, so a zero request delta proves it
// neither queried for external copies nor destroyed anything. Remove the
// gate and dedupPack would call FindSceneRefsByStashID (a request), failing
// this test — which is exactly what makes the gate load-bearing.
func TestDedupPackGateBlocksUnverified(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()
	if sc == nil {
		t.Fatal("rig stash client is nil")
	}
	g := &grabs.Grab{ID: 1, PackFiles: 0} // 0 ⇒ coverage unverifiable
	packScenes := []stash.SceneMatch{{ID: "pack-1", StashDBID: "sdb-abc"}}

	before := r.stash.reqCount()
	deduped, recorded, err := r.poller.dedupPack(context.Background(), sc, g, packScenes, "https://stashdb.org/graphql", "pack", false)
	if err != nil {
		t.Fatalf("dedupPack: %v", err)
	}
	if deduped != 0 || recorded != 0 {
		t.Fatalf("gate should block all action, got deduped=%d recorded=%d", deduped, recorded)
	}
	if got := r.stash.reqCount() - before; got != 0 {
		t.Fatalf("blocked keep=pack must make zero Stash calls, made %d", got)
	}
}

// TestDedupPackVerifiedQueriesStash is the positive control for the gate:
// with coverage verified (PackFiles>0) the keep="pack" path proceeds and
// must look up external copies in Stash. The empty scene set means nothing
// is actually destroyed, but the non-zero request delta proves the gate is
// the ONLY thing suppressing the destructive path in the test above.
func TestDedupPackVerifiedQueriesStash(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()
	g := &grabs.Grab{ID: 1, PackFiles: 10}
	packScenes := []stash.SceneMatch{{ID: "pack-1", StashDBID: "sdb-abc"}}

	before := r.stash.reqCount()
	if _, _, err := r.poller.dedupPack(context.Background(), sc, g, packScenes, "https://stashdb.org/graphql", "pack", true); err != nil {
		t.Fatalf("dedupPack: %v", err)
	}
	if got := r.stash.reqCount() - before; got == 0 {
		t.Fatalf("verified keep=pack should query Stash for external copies, made 0 requests")
	}
}

// TestDedupPackReviewRecordsPending exercises keep="review": when the pack
// delivers a scene the library already had, review mode must destroy nothing
// and instead persist a pending duplicate (pack copy + the pre-existing copy)
// for the user to resolve. Proves the detection path through the repo,
// including the existing-copies JSON round-trip.
func TestDedupPackReviewRecordsPending(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()
	// Two library copies of the same StashDB scene: the pack's and a
	// pre-existing one. The fake returns both for the cross-id lookup.
	r.stash.set([]fakeScene{
		{id: "pack-1", title: "Scene A", path: "/lib/pack/a.mp4", stashDBID: "sdb-abc"},
		{id: "old-9", title: "Scene A", path: "/lib/old/a.mp4", stashDBID: "sdb-abc"},
	})
	g := &grabs.Grab{ID: 1, PackFiles: 1}
	packScenes := []stash.SceneMatch{{ID: "pack-1", StashDBID: "sdb-abc", Title: "Scene A", FilePath: "/lib/pack/a.mp4"}}

	deduped, recorded, err := r.poller.dedupPack(context.Background(), sc, g, packScenes, "https://stashdb.org/graphql", "review", true)
	if err != nil {
		t.Fatalf("dedupPack: %v", err)
	}
	if deduped != 0 {
		t.Fatalf("review mode must destroy nothing, deduped=%d", deduped)
	}
	if recorded != 1 {
		t.Fatalf("expected 1 recorded review item, got %d", recorded)
	}

	dups, err := r.repo.PendingDuplicatesByGrab(context.Background(), 1)
	if err != nil {
		t.Fatalf("PendingDuplicatesByGrab: %v", err)
	}
	if len(dups) != 1 {
		t.Fatalf("expected 1 pending dup, got %d", len(dups))
	}
	d := dups[0]
	if d.Pack.SceneID != "pack-1" {
		t.Fatalf("pack copy = %q, want pack-1", d.Pack.SceneID)
	}
	if len(d.Existing) != 1 || d.Existing[0].SceneID != "old-9" {
		t.Fatalf("existing copies = %+v, want [old-9]", d.Existing)
	}
	if d.Status != "pending" {
		t.Fatalf("status = %q, want pending", d.Status)
	}
}
