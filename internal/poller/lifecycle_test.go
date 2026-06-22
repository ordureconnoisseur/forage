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
	mu        sync.Mutex
	scenes    []fakeScene
	reqs      int  // total GraphQL requests served (lets a test prove a code path made no calls)
	generated int  // metadataGenerate calls served (proves deferred preview/sprite generation fired)
	scanned   int  // metadataScan calls served (proves a (re-)scan fired)
	boxErr    bool // when true, stashBoxes queries 500 (simulates Stash unreachable for the endpoint lookup)
	jobErr    bool // when true, findJob queries 500 (simulates a JobStatus query failure)
	jobStatus string
}

func (f *fakeStash) set(scenes []fakeScene) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scenes = scenes
}

func (f *fakeStash) setBoxErr(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.boxErr = v
}

func (f *fakeStash) setJob(err bool, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobErr = err
	f.jobStatus = status
}

func (f *fakeStash) scanCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scanned
}

func (f *fakeStash) reqCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func (f *fakeStash) generateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generated
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
		case strings.Contains(q, "metadataGenerate"):
			f.mu.Lock()
			f.generated++
			f.mu.Unlock()
			writeRaw(w, `{"data":{"metadataGenerate":"generate-job-1"}}`)
		case strings.Contains(q, "metadataIdentify"):
			writeRaw(w, `{"data":{"metadataIdentify":"identify-job-1"}}`)
		case strings.Contains(q, "metadataScan"):
			f.mu.Lock()
			f.scanned++
			f.mu.Unlock()
			writeRaw(w, `{"data":{"metadataScan":"scan-job-1"}}`)
		case strings.Contains(q, "findJob"):
			f.mu.Lock()
			jobErr, status := f.jobErr, f.jobStatus
			f.mu.Unlock()
			if jobErr {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if status == "" {
				writeRaw(w, `{"data":{"findJob":null}}`)
				return
			}
			writeRaw(w, `{"data":{"findJob":{"status":"`+status+`"}}}`)
		case strings.Contains(q, "stashBoxes"):
			f.mu.Lock()
			boxErr := f.boxErr
			f.mu.Unlock()
			if boxErr {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
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
	sab     *fakeSab
	stash   *fakeStash
	libRoot string // <tmp>/library
	stage   string // <tmp>/staging (the download client's "complete" dir)
	cfg     config.Config
}

// fakeSab serves the two SAB endpoints the poller touches: mode=queue
// (always empty) and mode=history (configurable slots). Slots are raw
// maps so tests control exactly which JSON fields exist.
type fakeSab struct {
	mu    sync.Mutex
	slots []map[string]any
}

func (f *fakeSab) set(slots []map[string]any) {
	f.mu.Lock()
	f.slots = slots
	f.mu.Unlock()
}

func (f *fakeSab) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("mode") {
		case "queue":
			writeRaw(w, `{"queue":{"kbpersec":"0","slots":[]}}`)
		case "history":
			f.mu.Lock()
			slots := f.slots
			f.mu.Unlock()
			if slots == nil {
				slots = []map[string]any{}
			}
			writeJSON(w, map[string]any{"history": map[string]any{"slots": slots}})
		default:
			writeRaw(w, `{}`)
		}
	})
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
	fsab := &fakeSab{}
	fs := &fakeStash{}
	qSrv := httptest.NewServer(fq.handler())
	sabSrv := httptest.NewServer(fsab.handler())
	sSrv := httptest.NewServer(fs.handler())
	t.Cleanup(qSrv.Close)
	t.Cleanup(sabSrv.Close)
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

	cfg := config.Config{
		StashURL:     sSrv.URL,
		StashAPIKey:  "test-key",
		QbitURL:      qSrv.URL,
		QbitCategory: qbitCategory, // empty disables adoptOrphans
		SabURL:       sabSrv.URL,
		SabAPIKey:    "test-key",
		SabCategory:  qbitCategory, // mirrors qBit: empty disables SAB adoption
		LibraryRoot:  libRoot,
	}
	pool := clientpool.New()
	pool.Reload(cfg)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(repo, dbh, pool, log, time.Minute, 6*time.Hour)

	return &rig{poller: p, repo: repo, qbit: fq, sab: fsab, stash: fs, libRoot: libRoot, stage: stage, cfg: cfg}
}

// setConfig mutates the rig's config via fn and reloads the pool. Lets a test
// turn on a setting (e.g. a path mapping or a pack dedup mode) without
// re-specifying the httptest server URLs.
func (r *rig) setConfig(fn func(c *config.Config)) {
	fn(&r.cfg)
	r.poller.pool.Reload(r.cfg)
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

// TestAdoptedSingleRetriesIdentifyThenConfirms covers the fix for manual
// (no-prediction) singles: an adopted scene that's in Stash but not yet
// cross-id'd must NOT give up after a single identify attempt (the bug that
// left studio scenes like BLACKED unidentified when their queued identify ran
// late or was lost). It stays "scanned", retrying identify, and confirms as
// identified once the cross-id lands.
func TestAdoptedSingleRetriesIdentifyThenConfirms(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const performer = "Studio Performer"
	fileName := "BLACKED_TEST_1080P.mp4"
	placed := filepath.Join(r.libRoot, performer, fileName)
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "BLACKED test", Client: "qbit", ClientID: "blktesthash",
		Category: "forager", Status: "placed", PlacedPath: placed,
		PerformerName: performer, Kind: "single", Reason: "adopted from qbit",
		// no PredictedStashDBID — this is the manual/adopted path
		CompletedAt: time.Now().Unix(),
		GrabbedAt:   time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Scene is indexed in Stash but has no StashDB cross-id yet (identify
	// hasn't landed — e.g. queued behind a batch).
	r.stash.set([]fakeScene{{id: "100", title: "", path: placed, stashDBID: ""}})

	// Tick 1: placed -> scanned, fires identify. Must NOT confirm.
	r.tick(t)
	if g := r.get(t, id); g.Status != "scanned" {
		t.Fatalf("tick 1: status=%q, want scanned (reason=%q)", g.Status, g.Reason)
	}
	// Tick 2: still within grace, identify hasn't landed -> must STILL be
	// scanned, not prematurely confirmed "in library (scanned)" (the old bug).
	r.tick(t)
	if g := r.get(t, id); g.Status != "scanned" {
		t.Fatalf("tick 2: status=%q, want still scanned (one-shot give-up regression)", g.Status)
	}

	// Identify lands: the scene now carries a StashDB cross-id -> confirmed.
	r.stash.set([]fakeScene{{id: "100", title: "Tiny Fresh Face", path: placed, stashDBID: "sdb-blacked-1"}})
	r.tick(t)
	g := r.get(t, id)
	if g.Status != "confirmed" {
		t.Fatalf("after identify landed: status=%q, want confirmed (reason=%q)", g.Status, g.Reason)
	}
	if g.ActualStashDBID != "sdb-blacked-1" {
		t.Fatalf("actual_stashdb_id=%q, want the landed cross-id", g.ActualStashDBID)
	}
	// The fast placement scan skips previews/sprites; forage must generate them
	// once identify settles, so the scene gets the artifacts the scan deferred.
	if r.stash.generateCount() == 0 {
		t.Errorf("expected deferred preview/sprite generation to fire on confirm")
	}
}

// TestAdoptedSingleSettlesAfterGrace covers the other half: a no-prediction
// single whose scene genuinely isn't on StashDB (amateur content) must not
// retry forever — once singleIdentifyGrace passes it settles to confirmed
// "in library (scanned)".
func TestAdoptedSingleSettlesAfterGrace(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const performer = "Amateur Creator"
	fileName := "onlyfans_clip.mp4"
	placed := filepath.Join(r.libRoot, performer, fileName)
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "onlyfans clip", Client: "qbit", ClientID: "amateurhash",
		Category: "forager", Status: "scanned", PlacedPath: placed,
		PerformerName: performer, Kind: "single", Reason: "in Stash, awaiting identify",
		// grace measured from completion: set it past singleIdentifyGrace
		CompletedAt: time.Now().Add(-singleIdentifyGrace - time.Minute).Unix(),
		GrabbedAt:   time.Now().Add(-2 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// In Stash, never cross-id'd (StashDB doesn't have this amateur scene).
	r.stash.set([]fakeScene{{id: "200", title: "", path: placed, stashDBID: ""}})

	r.tick(t)
	g := r.get(t, id)
	if g.Status != "confirmed" {
		t.Fatalf("past grace: status=%q, want confirmed (reason=%q)", g.Status, g.Reason)
	}
	if g.Reason != "in library (scanned)" {
		t.Fatalf("reason=%q, want \"in library (scanned)\"", g.Reason)
	}
	// Even unidentified (amateur) scenes get the deferred previews/sprites —
	// the user wants generation on everything forage adds.
	if r.stash.generateCount() == 0 {
		t.Errorf("expected generation to fire even when settling unidentified")
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

// TestLifecycleAdoptionDeferredWithoutFileList: a magnet whose metadata
// hasn't resolved exposes no file list, so pack-vs-single can't be
// classified yet. Adoption must defer (kind is decided once and never
// revisited — guessing "single" routed packs down the single-scene
// confirm path forever), then adopt with the right kind once the file
// list exists.
func TestLifecycleAdoptionDeferredWithoutFileList(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const hash = "metadlhash000"
	addedOn := time.Now().Add(-10 * time.Minute).Unix()
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: "Some Performer Siterip", Category: "forager",
		State: "metaDL", Progress: 0, AddedOn: addedOn,
	}})
	// No file list — metadata still resolving.

	r.tick(t)
	known, err := r.repo.KnownClientIDs(ctx)
	if err != nil {
		t.Fatalf("known: %v", err)
	}
	if known[hash] {
		t.Fatalf("adopted despite missing file list — would have classified blind")
	}

	// Metadata lands: 5 videos → a pack.
	r.qbit.files[hash] = []qbit.TorrentFile{
		{Name: "a.mp4", Size: 1}, {Name: "b.mp4", Size: 1}, {Name: "c.mp4", Size: 1},
		{Name: "d.mp4", Size: 1}, {Name: "e.mp4", Size: 1},
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
		t.Fatalf("not adopted after file list appeared")
	}
	if adopted.Kind != "pack" || adopted.PackFiles != 5 {
		t.Fatalf("adopted kind=%q packFiles=%d, want pack/5", adopted.Kind, adopted.PackFiles)
	}
}

// TestLifecycleSabAdoption: untracked COMPLETED forage-category SAB
// history jobs become grab rows keyed on the nzo_id, classified from
// their on-disk storage (a directory of 3+ videos = pack). Jobs outside
// the sabAdoptWindow and non-Completed jobs are left alone.
func TestLifecycleSabAdoption(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	single := r.stageFile(t, "Manually Added Nzb.mkv")
	packDir := filepath.Join(r.stage, "Some Performer Pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir pack: %v", err)
	}
	for _, n := range []string{"a.mkv", "b.mkv", "c.mkv"} {
		if err := os.WriteFile(filepath.Join(packDir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write pack file: %v", err)
		}
	}
	ancient := r.stageFile(t, "Ancient.mkv")

	now := time.Now().Unix()
	r.sab.set([]map[string]any{
		{"nzo_id": "SABnzbd_nzo_new1", "name": "Manually Added Nzb", "category": "forager",
			"status": "Completed", "storage": single, "completed": now - 60},
		{"nzo_id": "SABnzbd_nzo_pack", "name": "Some Performer Pack", "category": "forager",
			"status": "Completed", "storage": packDir, "completed": now - 60},
		// Outside the adoption window — must not be re-imported.
		{"nzo_id": "SABnzbd_nzo_old", "name": "Ancient", "category": "forager",
			"status": "Completed", "storage": ancient, "completed": now - 3*24*3600},
		// Failed jobs are never adopted.
		{"nzo_id": "SABnzbd_nzo_fail", "name": "Broken Job", "category": "forager",
			"status": "Failed", "storage": "", "completed": now - 60},
	})

	r.tick(t)

	known, err := r.repo.KnownClientIDs(ctx)
	if err != nil {
		t.Fatalf("known: %v", err)
	}
	if !known["SABnzbd_nzo_new1"] || !known["SABnzbd_nzo_pack"] {
		t.Fatalf("completed jobs not adopted: known=%v", known)
	}
	if known["SABnzbd_nzo_old"] || known["SABnzbd_nzo_fail"] {
		t.Fatalf("old/failed jobs wrongly adopted: known=%v", known)
	}

	active, err := r.repo.Active(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	var pack *grabs.Grab
	for i := range active {
		if active[i].ClientID == "SABnzbd_nzo_pack" {
			pack = &active[i]
		}
	}
	if pack == nil {
		t.Fatalf("pack grab not active (active=%d)", len(active))
	}
	if pack.Client != "sabnzbd" || pack.Kind != "pack" || pack.PackFiles != 3 {
		t.Fatalf("pack grab client=%q kind=%q packFiles=%d, want sabnzbd/pack/3",
			pack.Client, pack.Kind, pack.PackFiles)
	}
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

// TestHealLeavesLegitimatePlacementAlone pins the two guards on the
// premature-placement heal: a file placed from a download that completed
// must survive (a) a force recheck, where qBit's progress is the
// verification fraction and reads < 1 for a fully-placed torrent, and
// (b) any later progress dip, because the placement was stamped at/after
// the recorded completion. Without the guards a tick landing mid-recheck
// RemoveAll'd the live library copy and regressed the grab.
func TestHealLeavesLegitimatePlacementAlone(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()

	const (
		hash      = "legit00hash"
		performer = "Hazel Moore"
		fileName  = "complete.mkv"
	)
	placedDir := filepath.Join(r.libRoot, performer)
	if err := os.MkdirAll(placedDir, 0o755); err != nil {
		t.Fatalf("mkdir placed: %v", err)
	}
	placedPath := filepath.Join(placedDir, fileName)
	if err := os.WriteFile(placedPath, []byte("a whole file"), 0o644); err != nil {
		t.Fatalf("write placed: %v", err)
	}

	now := time.Now().Unix()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Hazel Moore - Release", Client: "qbit", ClientID: hash,
		Category: "forager", Status: "placed", PlacedPath: placedPath,
		// Placement happened AFTER the download was seen complete — the
		// ordering that marks it legitimate.
		CompletedAt: now - 60, PlacedAt: now - 30,
		PerformerName: performer, Kind: "single",
		Progress: 1, GrabbedAt: now - 120,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// (a) Mid-recheck: progress is the verification fraction.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: fileName, Category: "forager",
		State: "checkingUP", Progress: 0.4,
	}})
	r.tick(t)
	g := r.get(t, id)
	if g.PlacedPath != placedPath {
		t.Fatalf("recheck: placed_path=%q, want untouched %q", g.PlacedPath, placedPath)
	}
	if _, err := os.Stat(placedPath); err != nil {
		t.Fatalf("recheck: placed file removed: %v", err)
	}

	// (b) Recheck "finished" below 1 (lost pieces): the completed-then-
	// placed ordering still protects the library copy.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: fileName, Category: "forager",
		State: "downloading", Progress: 0.4,
	}})
	r.tick(t)
	g = r.get(t, id)
	if g.PlacedPath != placedPath {
		t.Fatalf("post-recheck: placed_path=%q, want untouched %q", g.PlacedPath, placedPath)
	}
	if _, err := os.Stat(placedPath); err != nil {
		t.Fatalf("post-recheck: placed file removed: %v", err)
	}
}

// TestQueuedGrabSurvivesHashNotYetInQbit pins the async-add grace: a
// queued grab pins its info-hash at insert/retry time but the actual add
// runs behind the fetch gate, so the hash being absent from qBit must
// not fail the grab inside the link window (the add may still be in
// flight; failing strands the torrent when it lands, since failed grabs
// leave Active() and KnownClientIDs blocks adoption). Past the window it
// fails as before. The second half also exercises grabbed_at
// persistence through Repo.Update — the field used to be missing from
// the SET list, so retry's re-arm was silently dropped.
func TestQueuedGrabSurvivesHashNotYetInQbit(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()

	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Hazel Moore - Release", Client: "qbit",
		ClientID: "notyetadded00hash", Category: "forager",
		Status: "queued", Kind: "single", GrabbedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// qBit doesn't know the hash yet (empty torrent list) — fresh grab
	// stays queued.
	r.qbit.set(nil)
	r.tick(t)
	if g := r.get(t, id); g.Status != "queued" {
		t.Fatalf("inside grace: status=%q (reason=%q), want queued", g.Status, g.Reason)
	}

	// Age the grab past the link window (round-tripped through Update,
	// which must persist grabbed_at for this to take effect) — now the
	// missing hash means the add really died.
	g := r.get(t, id)
	g.GrabbedAt = time.Now().Add(-qbitLinkTimeout - time.Minute).Unix()
	if err := r.repo.Update(ctx, *g); err != nil {
		t.Fatalf("update: %v", err)
	}
	r.tick(t)
	if g := r.get(t, id); g.Status != "failed" {
		t.Fatalf("past grace: status=%q (reason=%q), want failed", g.Status, g.Reason)
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

// TestSabCompletedWedgeFailsWhenHistoryGone: a completed-but-unplaced SAB
// grab whose history entry is gone can never be placed (the entry's Path
// was the only source of the on-disk location), so past the registration
// grace it must fail (retryable) instead of sitting at "completed" forever.
func TestSabCompletedWedgeFailsWhenHistoryGone(t *testing.T) {
	r := newRig(t, "") // rig's LibraryRoot makes the placer Configured()
	ctx := context.Background()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Performer - Scene",
		Client:       "sabnzbd",
		ClientID:     "SABnzbd_nzo_x1",
		Status:       "completed",
		GrabbedAt:    time.Now().Add(-time.Hour).Unix(),
		CompletedAt:  time.Now().Add(-10 * time.Minute).Unix(), // past sabRegisterGrace
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	g := r.get(t, id)

	// Lists fetched fine this tick, entry just isn't in them any more.
	if err := r.poller.advance(ctx, g, nil, nil, false, nil, nil, nil, true); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got := r.get(t, id)
	if got.Status != "failed" {
		t.Fatalf("status = %q (reason %q), want failed", got.Status, got.Reason)
	}
}

// TestSabCompletedFreshSurvivesHistoryBlip: within the registration grace a
// missing history entry can be the queue->history handoff mid-flap, so a
// just-completed grab must be left alone.
func TestSabCompletedFreshSurvivesHistoryBlip(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Performer - Scene",
		Client:       "sabnzbd",
		ClientID:     "SABnzbd_nzo_x2",
		Status:       "completed",
		GrabbedAt:    time.Now().Unix(),
		CompletedAt:  time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	g := r.get(t, id)
	if err := r.poller.advance(ctx, g, nil, nil, false, nil, nil, nil, true); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := r.get(t, id); got.Status != "completed" {
		t.Fatalf("status = %q, want completed (inside grace)", got.Status)
	}
}

// TestSabFetchErrorFreezesGrabs: when the tick's SAB queue/history fetch
// failed, the empty lists must NOT be read as "SAB lost the download" — the
// grab is left untouched until a tick with good lists.
func TestSabFetchErrorFreezesGrabs(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Performer - Scene",
		Client:       "sabnzbd",
		ClientID:     "SABnzbd_nzo_x3",
		Status:       "downloading",
		GrabbedAt:    time.Now().Add(-time.Hour).Unix(), // long past every grace
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	g := r.get(t, id)
	if err := r.poller.advance(ctx, g, nil, nil, false, nil, nil, nil, false); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := r.get(t, id); got.Status != "downloading" {
		t.Fatalf("status = %q, want downloading (frozen on fetch error)", got.Status)
	}
}

// TestSabInflightSurvivesQueueGap pins the prevention half of the false-fail
// fix: a SAB grab temporarily absent from BOTH queue and history (SAB fetching
// the NZB from the indexer, or the job waiting in a backed-up queue) must NOT
// be failed on the first miss — even long past the old 5-minute grab-time
// grace — only after sabInflightTimeout of CONTINUOUS no-contact.
func TestSabInflightSurvivesQueueGap(t *testing.T) {
	r := newRig(t, "")
	ctx := context.Background()
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Performer - Scene",
		Client:       "sabnzbd",
		ClientID:     "SABnzbd_nzo_gap",
		Status:       "downloading",
		GrabbedAt:    time.Now().Add(-time.Hour).Unix(), // long past sabRegisterGrace
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// First miss: lists fetched fine (sabListsOK=true), nzo just isn't in
	// either yet. Must seed the absence clock and leave the grab alone.
	g := r.get(t, id)
	if err := r.poller.advance(ctx, g, nil, nil, false, nil, nil, nil, true); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := r.get(t, id); got.Status != "downloading" {
		t.Fatalf("status = %q, want downloading (first miss must not fail)", got.Status)
	}

	// Once contact has been lost for the full inflight timeout, it does fail
	// (a genuinely removed job). Backdate the recorded contact to simulate it.
	r.poller.graceMu.Lock()
	r.poller.grace[id] = time.Now().Add(-sabInflightTimeout - time.Minute)
	r.poller.graceMu.Unlock()
	g = r.get(t, id)
	if err := r.poller.advance(ctx, g, nil, nil, false, nil, nil, nil, true); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got := r.get(t, id); got.Status != "failed" {
		t.Fatalf("status = %q, want failed (past inflight timeout)", got.Status)
	}
}

// TestSabReviveFalseFailed pins the recovery half: a SAB grab wrongly marked
// failed (download still unplaced) is flipped back to "completed" when the
// adopt sweep sees its nzo Completed in SAB history, so the place pipeline can
// finish what the spurious failure interrupted. Without this, adoption skips
// the nzo as already-known and the finished download is stranded forever.
func TestSabReviveFalseFailed(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	file := r.stageFile(t, "Recovered Scene.mkv")
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Performer - Recovered Scene",
		Client:       "sabnzbd",
		ClientID:     "SABnzbd_nzo_recover",
		Category:     "forager",
		Status:       "failed",
		Reason:       "sab no longer tracks this nzo_id",
		GrabbedAt:    time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	now := time.Now().Unix()
	r.sab.set([]map[string]any{
		{"nzo_id": "SABnzbd_nzo_recover", "name": "Recovered Scene", "category": "forager",
			"status": "Completed", "storage": file, "completed": now - 60},
	})

	// Revival is NOT an adoption — the nzo is already known, so nothing new is
	// inserted; AdoptNow's count must stay 0.
	if n, _ := r.poller.AdoptNow(ctx); n != 0 {
		t.Fatalf("AdoptNow adopted=%d, want 0 (revive is not adoption)", n)
	}

	got := r.get(t, id)
	if got.Status != "completed" {
		t.Fatalf("status = %q (reason %q), want completed (revived)", got.Status, got.Reason)
	}
	if got.CompletedAt == 0 {
		t.Fatalf("completed_at not stamped on revive")
	}
	if !strings.Contains(got.Reason, "recovered") {
		t.Fatalf("reason = %q, want it to mention recovery", got.Reason)
	}
}

// TestQbitTransientErrorDoesNotFailImmediately covers the grace window that
// stops a momentary qBit "error" from stranding a download. qBit raises error
// transiently (tracker hiccup, brief disk/IO error, a recheck); failing on the
// first sighting drops the grab out of Active() so a torrent that recovers is
// never re-checked. The grab must stay downloading on the first error tick and
// only fail once the error has persisted past qbitErrorGrace.
func TestQbitTransientErrorDoesNotFailImmediately(t *testing.T) {
	r := newRig(t, "") // adoption off — exercise advanceQbit in isolation
	ctx := context.Background()

	const hash = "qbiterrorgracehash"
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Performer - Flapping Torrent",
		Client:       "qbit",
		ClientID:     hash,
		Category:     "forager",
		Status:       "downloading",
		Progress:     0.5,
		GrabbedAt:    time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// qBit reports a transient error on an otherwise mid-flight download.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: "Flapping Torrent", Category: "forager",
		State: "error", Progress: 0.5,
	}})

	// First sighting only starts the grace clock — the grab must NOT fail.
	r.tick(t)
	if g := r.get(t, id); g.Status != "downloading" {
		t.Fatalf("transient error within grace: status=%q (reason %q), want downloading", g.Status, g.Reason)
	}

	// Backdate the grace clock past qbitErrorGrace; the persisting error is now
	// terminal.
	r.poller.graceMu.Lock()
	r.poller.grace[id] = time.Now().Add(-qbitErrorGrace - time.Minute)
	r.poller.graceMu.Unlock()
	r.tick(t)
	if g := r.get(t, id); g.Status != "failed" {
		t.Fatalf("error persisted past grace: status=%q, want failed", g.Status)
	}
}

// TestQbitReviveFalseFailed covers the recovery path for a grab we DID fail
// (e.g. before this fix shipped, or an error that outlasted the grace) whose
// qBit torrent is in fact still present and healthy again. The adopt sweep
// must flip it back into the pipeline rather than leave it stranded — Active()
// excludes failed grabs and adoption skips known hashes, so without this the
// completed download is never placed. This is the qBit twin of
// TestSabReviveFalseFailed.
func TestQbitReviveFalseFailed(t *testing.T) {
	r := newRig(t, "forager")
	ctx := context.Background()

	const hash = "qbitrevivehash"
	id, err := r.repo.Insert(ctx, grabs.Grab{
		ReleaseTitle: "Performer - Recovered Torrent",
		Client:       "qbit",
		ClientID:     hash,
		Category:     "forager",
		Status:       "failed",
		Reason:       "qbit state=error",
		GrabbedAt:    time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// qBit still has the torrent and it's downloading again.
	r.qbit.set([]qbit.Torrent{{
		Hash: hash, Name: "Recovered Torrent", Category: "forager",
		State: "downloading", Progress: 0.4,
	}})

	// Revival is NOT an adoption — the hash is already known, so nothing new is
	// inserted; AdoptNow's count must stay 0.
	if n, _ := r.poller.AdoptNow(ctx); n != 0 {
		t.Fatalf("AdoptNow adopted=%d, want 0 (revive is not adoption)", n)
	}

	got := r.get(t, id)
	if got.Status != "downloading" {
		t.Fatalf("status = %q (reason %q), want downloading (revived)", got.Status, got.Reason)
	}
	if !strings.Contains(got.Reason, "recovered") {
		t.Fatalf("reason = %q, want it to mention recovery", got.Reason)
	}
}

// TestPackOrphansWhenStashNeverIndexes pins C1: a pack whose files Stash
// never indexes under the placed path (found==0, e.g. a stale path mapping)
// must orphan once the download is older than the orphan window, instead of
// re-scanning forever. The single-scene path always had this backstop; the
// pack path returned before reaching it.
func TestPackOrphansWhenStashNeverIndexes(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()
	r.stash.set(nil) // nothing indexed under the path

	g := &grabs.Grab{
		ID:          1,
		Kind:        "pack",
		Status:      "scanned",
		PlacedPath:  "/lib/Performer/SomePack",
		CompletedAt: time.Now().Add(-7 * time.Hour).Unix(), // older than the 6h orphan window
		PackFiles:   10,
	}
	dirty, err := r.poller.advancePackConfirm(context.Background(), g, sc)
	if err != nil {
		t.Fatalf("advancePackConfirm: %v", err)
	}
	if !dirty {
		t.Fatalf("expected a status change (dirty), got false")
	}
	if g.Status != "orphaned" {
		t.Fatalf("status = %q, want orphaned", g.Status)
	}

	// Before the orphan window elapses it must NOT orphan — it keeps trying.
	g2 := &grabs.Grab{
		ID:          2,
		Kind:        "pack",
		Status:      "scanned",
		PlacedPath:  "/lib/Performer/SomePack",
		CompletedAt: time.Now().Add(-1 * time.Minute).Unix(),
		PackFiles:   10,
	}
	if _, err := r.poller.advancePackConfirm(context.Background(), g2, sc); err != nil {
		t.Fatalf("advancePackConfirm g2: %v", err)
	}
	if g2.Status == "orphaned" {
		t.Fatalf("a recently-completed pack must not orphan yet, got %q", g2.Status)
	}
}

// TestPackRescansWhenSettledBelowCoverage pins C7: when Stash's scan settles
// below the expected file count (a coalesced/interrupted post-placement
// scan), the pack must re-fire the directory scan rather than stranding at
// "scanned" until the 6h backstop confirms against the partial set.
func TestPackRescansWhenSettledBelowCoverage(t *testing.T) {
	r := newRig(t, "")
	// Path mapping so triggerPlacementScan actually issues metadataScan
	// (it skips, without scanning, when a placed path can't be mapped).
	r.setConfig(func(c *config.Config) { c.StashPathMapping = "/lib:/lib" })
	sc := r.poller.pool.Stash()

	// 4 of an expected 10 files indexed (40% < the 80% floor).
	r.stash.set([]fakeScene{
		{id: "s1", title: "A", path: "/lib/P/Pack/a.mp4", stashDBID: "x1"},
		{id: "s2", title: "B", path: "/lib/P/Pack/b.mp4", stashDBID: "x2"},
		{id: "s3", title: "C", path: "/lib/P/Pack/c.mp4", stashDBID: "x3"},
		{id: "s4", title: "D", path: "/lib/P/Pack/d.mp4", stashDBID: "x4"},
	})
	g := &grabs.Grab{
		ID: 1, Kind: "pack", Status: "scanned",
		PlacedPath:  "/lib/P/Pack",
		CompletedAt: time.Now().Add(-1 * time.Minute).Unix(),
		PackFiles:   10,
	}
	// Force the scan to read as settled at the current count: seed the
	// high-water in the past so packScanStableWindow has elapsed.
	r.poller.packMu.Lock()
	r.poller.packScan[g.ID] = packScanState{count: 4, since: time.Now().Add(-10 * time.Minute)}
	r.poller.packMu.Unlock()

	before := r.stash.scanCount()
	if _, err := r.poller.advancePackConfirm(context.Background(), g, sc); err != nil {
		t.Fatalf("advancePackConfirm: %v", err)
	}
	if r.stash.scanCount() == before {
		t.Fatalf("expected a re-scan for a settled-but-incomplete pack, none fired")
	}
	if g.Status == "confirmed" {
		t.Fatalf("pack must not confirm below the coverage floor, got %q", g.Status)
	}
}

// TestPackDefersConfirmWhenEndpointLookupFails pins C10: a pack that is ready
// to confirm must NOT confirm when the stash-box endpoint lookup fails (Stash
// briefly unreachable). Confirming would run — or skip — dedup against an
// unreachable Stash and then leave Active() permanently. It must defer and
// confirm on a later tick once Stash is reachable again.
func TestPackDefersConfirmWhenEndpointLookupFails(t *testing.T) {
	r := newRig(t, "")
	r.setConfig(func(c *config.Config) { c.StashPathMapping = "/lib:/lib" })
	sc := r.poller.pool.Stash()

	// A fully-indexed, fully-identified 2-file pack: ready to confirm.
	r.stash.set([]fakeScene{
		{id: "s1", title: "A", path: "/lib/P/Pack/a.mp4", stashDBID: "x1"},
		{id: "s2", title: "B", path: "/lib/P/Pack/b.mp4", stashDBID: "x2"},
	})
	newPack := func(id int64) *grabs.Grab {
		g := &grabs.Grab{
			ID: id, Kind: "pack", Status: "scanned",
			PlacedPath:  "/lib/P/Pack",
			CompletedAt: time.Now().Add(-1 * time.Minute).Unix(),
			PackFiles:   2,
		}
		r.poller.packMu.Lock()
		r.poller.packScan[id] = packScanState{count: 2, since: time.Now().Add(-10 * time.Minute)}
		r.poller.packMu.Unlock()
		return g
	}

	// Endpoint lookup fails → defer (stay scanned).
	r.stash.setBoxErr(true)
	g := newPack(1)
	if _, err := r.poller.advancePackConfirm(context.Background(), g, sc); err != nil {
		t.Fatalf("advancePackConfirm (boxErr): %v", err)
	}
	if g.Status != "scanned" {
		t.Fatalf("status = %q, want scanned (deferred) while endpoint lookup fails", g.Status)
	}

	// Stash recovers → the same conditions now confirm.
	r.stash.setBoxErr(false)
	g2 := newPack(2)
	if _, err := r.poller.advancePackConfirm(context.Background(), g2, sc); err != nil {
		t.Fatalf("advancePackConfirm (recovered): %v", err)
	}
	if g2.Status != "confirmed" {
		t.Fatalf("status = %q, want confirmed once Stash is reachable", g2.Status)
	}
}

// TestIdentifyInFlightAssumesInFlightOnError pins C12: a JobStatus query
// error must be treated as "still in flight" (return true) so a transient
// blip can't stack a redundant Identify behind the still-pending one. A
// clean "job drained from the queue" reply (null) returns false so a genuine
// next batch can fire.
func TestIdentifyInFlightAssumesInFlightOnError(t *testing.T) {
	r := newRig(t, "")
	sc := r.poller.pool.Stash()
	ctx := context.Background()

	// No remembered job → not in flight.
	if r.poller.identifyInFlight(ctx, sc, 1) {
		t.Fatalf("no remembered job should not be in flight")
	}

	r.poller.rememberIdentifyJob(1, "identify-job-1")

	// JobStatus errors → assume still in flight (don't re-fire).
	r.stash.setJob(true, "")
	if !r.poller.identifyInFlight(ctx, sc, 1) {
		t.Fatalf("a JobStatus error must be treated as in flight")
	}

	// Job still running → in flight.
	r.stash.setJob(false, "RUNNING")
	if !r.poller.identifyInFlight(ctx, sc, 1) {
		t.Fatalf("a RUNNING job must be in flight")
	}

	// Job drained from the queue (null) → free to fire again.
	r.stash.setJob(false, "")
	if r.poller.identifyInFlight(ctx, sc, 1) {
		t.Fatalf("a drained/finished job must not be in flight")
	}
}
