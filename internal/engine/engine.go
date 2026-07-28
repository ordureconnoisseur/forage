// Package engine is forage's built-in torrent client — the piece that
// makes the one-click story complete: with no qBittorrent anywhere, grabs
// download inside the daemon itself (anacrolix/torrent) into the same
// download folder the placer hardlinks from, and the seeding cull reads
// the same ratio/age numbers it reads from qBit.
//
// The engine deliberately speaks qBit's dialect at its API boundary — it
// returns qbit.Torrent / qbit.TorrentFile shapes and mirrors the method
// set the poller and API layers already consume (ListTorrents,
// TorrentInfo, TorrentFiles, AddTorrent, AddTorrentFile, DeleteTorrent,
// Resume, EnsureCategory) — so swapping which client a grab rides is a
// pool-level decision, not a rewrite of the state machine.
//
// Persistence: metainfo files under <root>/torrents plus an
// engine_torrents DB row per torrent. On Start every saved torrent is
// re-added; piece completion lives in a bolt DB under <root> so a restart
// re-verifies nothing. uploaded/seed_seconds accumulate across restarts
// via the accounting loop (session stats reset every boot; the DB rows
// are the durable truth the cull reads).
package engine

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// Engine owns one anacrolix client session plus the durable accounting.
type Engine struct {
	root    string // <data>/engine — metainfo store, piece completion
	dataDir string // payload destination (the configured download folder)
	db      *sql.DB
	log     *slog.Logger

	mu     sync.Mutex
	cl     *torrent.Client
	store  storage.ClientImplCloser // owns the piece-completion DB; must close after cl
	closed bool
	// quiet strips the client down to direct-peer transfers only (no DHT,
	// trackers, or fixed listen port) — set by tests, where two engines in
	// one process would otherwise race for the default port and spray the
	// network.
	quiet bool
	// prev per-torrent session counters, for delta accounting.
	prevUp   map[string]int64
	prevDown map[string]int64
	speed    map[string]int64 // bytes/sec, from the last accounting tick
	lastTick time.Time
}

// New builds an engine rooted at dataDir(root)/engine writing payloads to
// downloadDir. Nothing starts until Start.
func New(root, downloadDir string, db *sql.DB, log *slog.Logger) *Engine {
	return &Engine{
		root:     filepath.Join(root, "engine"),
		dataDir:  downloadDir,
		db:       db,
		log:      log,
		prevUp:   map[string]int64{},
		prevDown: map[string]int64{},
		speed:    map[string]int64{},
	}
}

func (e *Engine) torrentsDir() string { return filepath.Join(e.root, "torrents") }

// Start opens the torrent client and re-adds every persisted torrent.
// Piece completion is durable, so resumes verify nothing.
func (e *Engine) Start(ctx context.Context) error {
	if e.dataDir == "" {
		return fmt.Errorf("engine needs a download folder")
	}
	if err := os.MkdirAll(e.torrentsDir(), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(e.dataDir, 0o755); err != nil {
		return err
	}
	pc, err := storage.NewDefaultPieceCompletionForDir(e.root)
	if err != nil {
		return fmt.Errorf("piece completion store: %w", err)
	}
	store := storage.NewFileWithCompletion(e.dataDir, pc)
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = e.dataDir
	cfg.Seed = true
	cfg.DefaultStorage = store
	if e.quiet {
		cfg.NoDHT = true
		cfg.DisableTrackers = true
		cfg.ListenPort = 0
		cfg.NoDefaultPortForwarding = true
	}
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("start torrent client: %w", err)
	}
	e.mu.Lock()
	e.cl = cl
	e.store = store
	e.lastTick = time.Now()
	e.mu.Unlock()

	// Re-add persisted torrents. Failures are per-torrent: one corrupt
	// metainfo must not keep the rest from seeding.
	entries, _ := os.ReadDir(e.torrentsDir())
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".torrent") {
			continue
		}
		mi, err := metainfo.LoadFromFile(filepath.Join(e.torrentsDir(), ent.Name()))
		if err != nil {
			e.log.Error("engine: load persisted metainfo", "file", ent.Name(), "err", err)
			continue
		}
		t, err := cl.AddTorrent(mi)
		if err != nil {
			e.log.Error("engine: re-add torrent", "file", ent.Name(), "err", err)
			continue
		}
		go e.downloadWhenReady(ctx, t)
	}
	e.log.Info("torrent engine started", "dataDir", e.dataDir, "resumed", len(entries))
	return nil
}

// EnsureStarted starts the engine bound to downloadDir unless it already
// runs. Returns whether THIS call started it (the caller then owns
// launching Run). A different dir on an already-running engine is noted
// but applies only at next daemon start — live torrents are mid-write in
// the old one.
func (e *Engine) EnsureStarted(ctx context.Context, downloadDir string) (bool, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return false, fmt.Errorf("torrent engine already shut down")
	}
	if e.cl != nil {
		if e.dataDir != downloadDir {
			e.log.Warn("engine download dir changed; restart the daemon to apply",
				"active", e.dataDir, "new", downloadDir)
		}
		e.mu.Unlock()
		return false, nil
	}
	e.dataDir = downloadDir
	e.mu.Unlock()
	return true, e.Start(ctx)
}

// Run drives the accounting loop until ctx ends, then closes the client.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			e.Close()
			return
		case <-tick.C:
			e.account(ctx)
		}
	}
}

// Close shuts the torrent session down (final accounting pass included).
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	cl, store := e.cl, e.store
	e.mu.Unlock()
	if cl != nil {
		e.account(context.Background())
		cl.Close()
	}
	if store != nil {
		// Releases the piece-completion bolt DB's file lock — without this
		// a restart in the same process (or the next daemon boot on some
		// filesystems) times out trying to reopen it.
		_ = store.Close()
	}
}

func (e *Engine) client() (*torrent.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cl == nil || e.closed {
		return nil, fmt.Errorf("torrent engine is not running (%w)", clienterr.ErrTransient)
	}
	return e.cl, nil
}

// downloadWhenReady waits for metadata then starts the payload download.
func (e *Engine) downloadWhenReady(ctx context.Context, t *torrent.Torrent) {
	select {
	case <-ctx.Done():
		return
	case <-t.GotInfo():
	}
	// Persist name/size now that the metadata is known (magnet adds don't
	// have it at insert time).
	_, _ = e.db.Exec(`UPDATE engine_torrents SET name = ?, total_size = ? WHERE info_hash = ? AND name = ''`,
		t.Name(), t.Length(), t.InfoHash().HexString())
	t.DownloadAll()
}

// AddTorrentFile registers raw .torrent bytes and starts downloading.
// Mirrors qbit.Client.AddTorrentFile.
func (e *Engine) AddTorrentFile(ctx context.Context, data []byte, category string) error {
	cl, err := e.client()
	if err != nil {
		return err
	}
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parse torrent: %w (%w)", err, clienterr.ErrRejected)
	}
	hash := mi.HashInfoBytes().HexString()
	if err := os.WriteFile(filepath.Join(e.torrentsDir(), hash+".torrent"), data, 0o644); err != nil {
		return fmt.Errorf("persist metainfo: %w", err)
	}
	t, err := cl.AddTorrent(mi)
	if err != nil {
		return fmt.Errorf("add torrent: %w", err)
	}
	name, size := "", int64(0)
	if t.Info() != nil {
		name, size = t.Name(), t.Length()
	}
	if err := e.insertRow(ctx, hash, name, category, size); err != nil {
		return err
	}
	go e.downloadWhenReady(context.WithoutCancel(ctx), t)
	return nil
}

// AddTorrent mirrors qbit.Client.AddTorrent: a magnet link goes straight
// in; an http(s) URL is fetched and delegated to AddTorrentFile. Returns
// the info-hash — unlike qBit, the engine always knows it synchronously,
// which lets the caller skip the add-time-window matching dance.
func (e *Engine) AddTorrent(ctx context.Context, downloadURL, category string) (string, error) {
	if strings.HasPrefix(downloadURL, "magnet:") {
		cl, err := e.client()
		if err != nil {
			return "", err
		}
		t, err := cl.AddMagnet(downloadURL)
		if err != nil {
			return "", fmt.Errorf("add magnet: %w (%w)", err, clienterr.ErrRejected)
		}
		hash := t.InfoHash().HexString()
		if err := e.insertRow(ctx, hash, "", category, 0); err != nil {
			return "", err
		}
		// Persist the metainfo once metadata arrives so the magnet
		// survives a restart.
		go func() {
			select {
			case <-t.GotInfo():
				mi := t.Metainfo()
				var buf bytes.Buffer
				if err := mi.Write(&buf); err == nil {
					_ = os.WriteFile(filepath.Join(e.torrentsDir(), hash+".torrent"), buf.Bytes(), 0o644)
				}
			case <-time.After(30 * time.Minute):
			}
		}()
		go e.downloadWhenReady(context.WithoutCancel(ctx), t)
		return hash, nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return "", clienterr.Transport("engine torrent fetch", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", clienterr.Status("engine torrent fetch", resp.StatusCode, body)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", clienterr.Transport("engine torrent fetch read", err)
	}
	mi, err := metainfo.Load(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("parse fetched torrent: %w (%w)", err, clienterr.ErrRejected)
	}
	hash := mi.HashInfoBytes().HexString()
	if err := e.AddTorrentFile(ctx, raw, category); err != nil {
		return "", err
	}
	return hash, nil
}

func (e *Engine) insertRow(ctx context.Context, hash, name, category string, size int64) error {
	_, err := e.db.ExecContext(ctx, `
INSERT INTO engine_torrents (info_hash, name, category, added_on, total_size)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(info_hash) DO NOTHING`, hash, name, category, time.Now().Unix(), size)
	return err
}

// row is the durable half of a torrent's state.
type row struct {
	hash        string
	name        string
	category    string
	addedOn     int64
	completedOn int64
	totalSize   int64
	uploaded    int64
	seedSecs    int64
}

func (e *Engine) rows(ctx context.Context) (map[string]*row, error) {
	rs, err := e.db.QueryContext(ctx, `
SELECT info_hash, name, category, added_on, completed_on, total_size, uploaded, seed_seconds
  FROM engine_torrents`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]*row{}
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.hash, &r.name, &r.category, &r.addedOn, &r.completedOn, &r.totalSize, &r.uploaded, &r.seedSecs); err != nil {
			return nil, err
		}
		out[r.hash] = &r
	}
	return out, rs.Err()
}

// account folds each live torrent's session counters into its durable
// row: upload delta, seeding seconds while complete, completed_on when
// the payload first finishes. Also refreshes the speed map.
func (e *Engine) account(ctx context.Context) {
	e.mu.Lock()
	cl := e.cl
	elapsed := time.Since(e.lastTick)
	e.lastTick = time.Now()
	e.mu.Unlock()
	if cl == nil || elapsed <= 0 {
		return
	}
	for _, t := range cl.Torrents() {
		hash := t.InfoHash().HexString()
		st := t.Stats()
		up := st.ConnStats.BytesWrittenData.Int64()
		down := st.ConnStats.BytesReadData.Int64()

		e.mu.Lock()
		upDelta := up - e.prevUp[hash]
		downDelta := down - e.prevDown[hash]
		if upDelta < 0 {
			upDelta = 0 // fresh session counter
		}
		e.prevUp[hash] = up
		e.prevDown[hash] = down
		if downDelta > 0 {
			e.speed[hash] = int64(float64(downDelta) / elapsed.Seconds())
		} else {
			e.speed[hash] = 0
		}
		e.mu.Unlock()

		complete := t.Info() != nil && t.BytesCompleted() == t.Length()
		seedDelta := int64(0)
		if complete {
			seedDelta = int64(elapsed.Seconds())
		}
		_, err := e.db.ExecContext(ctx, `
UPDATE engine_torrents
   SET uploaded = uploaded + ?,
       seed_seconds = seed_seconds + ?,
       completed_on = CASE WHEN completed_on = 0 AND ? THEN ? ELSE completed_on END
 WHERE info_hash = ?`,
			upDelta, seedDelta, complete, time.Now().Unix(), hash)
		if err != nil {
			e.log.Error("engine accounting", "hash", hash, "err", err)
		}
	}
}

// ListTorrents mirrors qbit.Client.ListTorrents: the engine's torrents in
// qBit's wire shape, filterable by category and hash set. Sort/limit are
// honoured in the qBit client for UI paging; the engine's population is
// small enough that callers' own handling suffices, so opts.Sort is
// ignored beyond added_on ordering.
func (e *Engine) ListTorrents(ctx context.Context, opts qbit.ListOpts) ([]qbit.Torrent, error) {
	cl, err := e.client()
	if err != nil {
		return nil, err
	}
	rows, err := e.rows(ctx)
	if err != nil {
		return nil, err
	}
	wantHash := map[string]bool{}
	for _, h := range opts.Hashes {
		wantHash[strings.ToLower(h)] = true
	}
	var out []qbit.Torrent
	for _, t := range cl.Torrents() {
		hash := t.InfoHash().HexString()
		if len(wantHash) > 0 && !wantHash[hash] {
			continue
		}
		r := rows[hash]
		if r == nil {
			continue // dropped row (delete raced the list) — not ours anymore
		}
		if opts.Category != "" && r.category != opts.Category {
			continue
		}
		out = append(out, e.wireTorrent(t, r))
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

// wireTorrent renders one live torrent in qBit's dialect. State strings
// are the subset of qBit's vocabulary the poller's classifier already
// understands — the version-matrix contract test in internal/poller pins
// that every state emitted by a client classifies.
func (e *Engine) wireTorrent(t *torrent.Torrent, r *row) qbit.Torrent {
	e.mu.Lock()
	speed := e.speed[t.InfoHash().HexString()]
	e.mu.Unlock()

	w := qbit.Torrent{
		Hash:         t.InfoHash().HexString(),
		Name:         r.name,
		Category:     r.category,
		AddedOn:      r.addedOn,
		Ratio:        0,
		CompletionOn: r.completedOn,
		SeedingTime:  r.seedSecs,
		Dlspeed:      speed,
	}
	if t.Info() == nil {
		w.State = "metaDL"
		return w
	}
	if w.Name == "" {
		w.Name = t.Name()
	}
	total := t.Length()
	done := t.BytesCompleted()
	if total > 0 {
		w.Progress = float64(done) / float64(total)
		w.Ratio = float64(r.uploaded) / float64(total)
	}
	w.ContentPath = filepath.Join(e.dataDir, t.Name())
	if done < total {
		if speed > 0 {
			w.State = "downloading"
			w.Eta = (total - done) / speed
		} else {
			w.State = "stalledDL"
			w.Eta = 8640000
		}
	} else {
		w.State = "uploading"
	}
	return w
}

// TorrentInfo mirrors qbit.Client.TorrentInfo, including the ErrNotFound
// contract for a hash the engine doesn't know.
func (e *Engine) TorrentInfo(ctx context.Context, hash string) (*qbit.Torrent, error) {
	if hash == "" {
		return nil, clienterr.ErrNotFound
	}
	ts, err := e.ListTorrents(ctx, qbit.ListOpts{Hashes: []string{strings.ToLower(hash)}})
	if err != nil {
		return nil, err
	}
	for i := range ts {
		if strings.EqualFold(ts[i].Hash, hash) {
			return &ts[i], nil
		}
	}
	return nil, clienterr.ErrNotFound
}

// TorrentFiles mirrors qbit.Client.TorrentFiles: display paths + sizes,
// available as soon as metadata is.
func (e *Engine) TorrentFiles(ctx context.Context, hash string) ([]qbit.TorrentFile, error) {
	if hash == "" {
		return nil, nil
	}
	cl, err := e.client()
	if err != nil {
		return nil, err
	}
	for _, t := range cl.Torrents() {
		if !strings.EqualFold(t.InfoHash().HexString(), hash) {
			continue
		}
		if t.Info() == nil {
			return nil, nil // metadata not here yet — same as qBit pre-metadata
		}
		var out []qbit.TorrentFile
		for _, f := range t.Files() {
			out = append(out, qbit.TorrentFile{Name: f.DisplayPath(), Size: f.Length()})
		}
		return out, nil
	}
	return nil, clienterr.ErrNotFound
}

// DeleteTorrent mirrors qbit.Client.DeleteTorrent: drop the torrent and,
// when deleteFiles is set, remove the engine's own payload copy (the
// library's hardlink is a separate inode reference and survives — same
// semantics as deleting from qBit).
func (e *Engine) DeleteTorrent(ctx context.Context, hash string, deleteFiles bool) error {
	cl, err := e.client()
	if err != nil {
		return err
	}
	hash = strings.ToLower(hash)
	var contentPath string
	for _, t := range cl.Torrents() {
		if strings.EqualFold(t.InfoHash().HexString(), hash) {
			if t.Info() != nil {
				contentPath = filepath.Join(e.dataDir, t.Name())
			}
			t.Drop()
			break
		}
	}
	if deleteFiles && contentPath != "" {
		// The payload path is derived from the torrent's own name; refuse
		// anything that doesn't resolve strictly inside the download dir
		// (a hostile name like ".." must not reach outward).
		rel, rerr := filepath.Rel(e.dataDir, contentPath)
		if rerr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("refusing to delete %q: outside the download dir", contentPath)
		}
		if err := os.RemoveAll(contentPath); err != nil {
			return fmt.Errorf("delete payload: %w", err)
		}
	}
	_ = os.Remove(filepath.Join(e.torrentsDir(), hash+".torrent"))
	if _, err := e.db.ExecContext(ctx, `DELETE FROM engine_torrents WHERE info_hash = ?`, hash); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.prevUp, hash)
	delete(e.prevDown, hash)
	delete(e.speed, hash)
	e.mu.Unlock()
	return nil
}

// Resume mirrors qbit.Client.Resume. Engine torrents are never paused by
// forage's flows, so this is a successful no-op — the call sites only use
// it to un-stick adopted qBit torrents.
func (e *Engine) Resume(ctx context.Context, hash string) error { return nil }

// EnsureCategory mirrors qbit.Client.EnsureCategory. The engine has no
// category config to create — every payload lands in its one download
// dir — so recording the request is enough for the call to be honest.
func (e *Engine) EnsureCategory(ctx context.Context, name, savePath string) error {
	if name == "" {
		return fmt.Errorf("category name is empty")
	}
	return nil
}

// Categories mirrors qbit.Client.Categories: the single implicit
// category, pointing at the download dir.
func (e *Engine) Categories(ctx context.Context) (map[string]qbit.Category, error) {
	return map[string]qbit.Category{
		"forage": {Name: "forage", SavePath: e.dataDir},
	}, nil
}
