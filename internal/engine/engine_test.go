package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/ordureconnoisseur/forager/internal/clienterr"
	"github.com/ordureconnoisseur/forager/internal/db"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// buildTorrent creates a payload file in dir and returns the raw
// .torrent bytes describing it.
func buildTorrent(t *testing.T, dir, name string, size int) []byte {
	t.Helper()
	payload := bytes.Repeat([]byte("forage-engine-test-"), size/19+1)[:size]
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 32 * 1024}
	if err := info.BuildFromFilePath(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestEngine(t *testing.T, root, dataDir string) *Engine {
	t.Helper()
	dbh, err := db.Open(filepath.Join(root, "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbh.Close() })
	e := New(root, dataDir, dbh, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.Quiet()
	return e
}

// TestEngineSeedToLeech is the end-to-end proof: engine A holds the
// payload and seeds; engine B, wired to A as a direct peer, downloads it,
// reports qBit-shaped progress, flips to a seed state on completion, and
// the accounting rows fill in.
func TestEngineSeedToLeech(t *testing.T) {
	ctx := context.Background()
	seedRoot, seedData := t.TempDir(), t.TempDir()
	leechRoot, leechData := t.TempDir(), t.TempDir()

	raw := buildTorrent(t, seedData, "payload.bin", 300*1024)

	seeder := newTestEngine(t, seedRoot, seedData)
	if err := seeder.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer seeder.Close()
	if err := seeder.AddTorrentFile(ctx, raw, "forage"); err != nil {
		t.Fatal(err)
	}
	// The payload already sits in the seeder's data dir; piece check makes
	// it complete without downloading.
	waitFor(t, 15*time.Second, func() bool {
		ts, err := seeder.ListTorrents(ctx, listAll())
		return err == nil && len(ts) == 1 && ts[0].Progress == 1
	}, "seeder never verified its payload")

	leecher := newTestEngine(t, leechRoot, leechData)
	if err := leecher.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer leecher.Close()
	if err := leecher.AddTorrentFile(ctx, raw, "forage"); err != nil {
		t.Fatal(err)
	}
	// Direct-peer the two clients (quiet mode has no DHT or trackers).
	for _, lt := range leecher.ClientForTest().Torrents() {
		lt.AddClientPeer(seeder.ClientForTest())
	}
	waitFor(t, 30*time.Second, func() bool {
		ts, err := leecher.ListTorrents(ctx, listAll())
		return err == nil && len(ts) == 1 && ts[0].Progress == 1
	}, "leecher never completed the transfer")

	// Payload byte-identical.
	want, _ := os.ReadFile(filepath.Join(seedData, "payload.bin"))
	got, err := os.ReadFile(filepath.Join(leechData, "payload.bin"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("payload mismatch after transfer: err=%v len=%d want=%d", err, len(got), len(want))
	}

	// qBit-dialect checks on the completed torrent.
	leecher.AccountForTest()
	ts, err := leecher.ListTorrents(ctx, listAll())
	if err != nil || len(ts) != 1 {
		t.Fatalf("list: %v (%d)", err, len(ts))
	}
	tor := ts[0]
	if tor.State != "uploading" {
		t.Errorf("state = %q, want uploading", tor.State)
	}
	if tor.CompletionOn == 0 {
		t.Error("completion_on not set by accounting")
	}
	if tor.ContentPath != filepath.Join(leechData, "payload.bin") {
		t.Errorf("content_path = %q", tor.ContentPath)
	}
	if tor.Category != "forage" {
		t.Errorf("category = %q", tor.Category)
	}

	// The seeder uploaded those bytes — its cumulative counter and ratio
	// must show it.
	seeder.AccountForTest()
	st, err := seeder.ListTorrents(ctx, listAll())
	if err != nil || len(st) != 1 {
		t.Fatalf("seeder list: %v", err)
	}
	if st[0].Ratio <= 0 {
		t.Errorf("seeder ratio = %v, want > 0 after uploading the payload", st[0].Ratio)
	}

	// TorrentFiles parity.
	files, err := leecher.TorrentFiles(ctx, tor.Hash)
	if err != nil || len(files) != 1 || files[0].Name != "payload.bin" || files[0].Size != 300*1024 {
		t.Errorf("files = %v, %v", files, err)
	}

	// Close both sessions and wait until Windows actually releases the
	// payload handles — TempDir cleanup fails on a still-open file even
	// after Close returns (the deferred Closes above stay as safety nets;
	// Close is idempotent).
	leecher.Close()
	seeder.Close()
	waitRemovable(t, filepath.Join(seedData, "payload.bin"))
	waitRemovable(t, filepath.Join(leechData, "payload.bin"))
}

// waitRemovable retries deleting path until the OS lets go of it.
func waitRemovable(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("%s still locked after close", path)
}

// TestEngineRestartResume: torrents survive a restart via the persisted
// metainfo + piece completion — no re-download, still complete.
func TestEngineRestartResume(t *testing.T) {
	ctx := context.Background()
	root, data := t.TempDir(), t.TempDir()
	raw := buildTorrent(t, data, "keep.bin", 64*1024)

	e := newTestEngine(t, root, data)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.AddTorrentFile(ctx, raw, "forage"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		ts, err := e.ListTorrents(ctx, listAll())
		return err == nil && len(ts) == 1 && ts[0].Progress == 1
	}, "payload never verified")
	e.AccountForTest()
	e.Close()

	e2 := newTestEngine(t, root, data)
	if err := e2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	waitFor(t, 15*time.Second, func() bool {
		ts, err := e2.ListTorrents(ctx, listAll())
		return err == nil && len(ts) == 1 && ts[0].Progress == 1 && ts[0].State == "uploading"
	}, "restart did not resume the torrent complete")
	ts, _ := e2.ListTorrents(ctx, listAll())
	if ts[0].CompletionOn == 0 {
		t.Error("completion_on lost across restart")
	}
}

// TestEngineDeleteTorrent: delete removes the row, the metainfo, and
// (with deleteFiles) the payload; TorrentInfo then reports ErrNotFound.
func TestEngineDeleteTorrent(t *testing.T) {
	ctx := context.Background()
	root, data := t.TempDir(), t.TempDir()
	raw := buildTorrent(t, data, "gone.bin", 64*1024)

	e := newTestEngine(t, root, data)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.AddTorrentFile(ctx, raw, "forage"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		ts, err := e.ListTorrents(ctx, listAll())
		return err == nil && len(ts) == 1 && ts[0].Progress == 1
	}, "payload never verified")
	ts, _ := e.ListTorrents(ctx, listAll())
	hash := ts[0].Hash

	if err := e.DeleteTorrent(ctx, hash, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "gone.bin")); !os.IsNotExist(err) {
		t.Error("payload survived deleteFiles=true")
	}
	if _, err := os.Stat(filepath.Join(root, "engine", "torrents", hash+".torrent")); !os.IsNotExist(err) {
		t.Error("metainfo survived delete")
	}
	if _, err := e.TorrentInfo(ctx, hash); !errors.Is(err, clienterr.ErrNotFound) {
		t.Errorf("TorrentInfo after delete = %v, want ErrNotFound", err)
	}
}

func listAll() qbit.ListOpts { return qbit.ListOpts{} }

func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal(msg)
}
