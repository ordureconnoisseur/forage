package poller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/config"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// cullRig: a rig with the cull thresholds set and one placed, tracked grab
// whose torrent sits in qBit — the canonical retirement candidate.
func cullRig(t *testing.T) (*rig, int64, string) {
	t.Helper()
	r := newRig(t, "forager")
	r.setConfig(func(c *config.Config) {
		c.SeedMaxAge = 7 * 24 * time.Hour
		c.SeedRatio = 1.0
	})
	placed := filepath.Join(r.libRoot, "P", "scene.mp4")
	if err := os.MkdirAll(filepath.Dir(placed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(placed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := r.repo.Insert(context.Background(), grabs.Grab{
		ReleaseTitle: "seeded scene", Client: "qbit", ClientID: "seedhash",
		Category: "forager", Status: "confirmed", PlacedPath: placed,
		Kind: "single", GrabbedAt: time.Now().Add(-30 * 24 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The cull consults the library-health latch, which tickOnce normally
	// arms. These tests drive the pass directly, so arm it here — without
	// this every test short-circuits at the latch and the guards under
	// test are never reached (the first version passed its negative cases
	// for exactly that wrong reason).
	r.poller.checkLibrary()
	return r, id, placed
}

func seededTorrent(ratio float64, seedingSecs int64) qbit.Torrent {
	return qbit.Torrent{
		Hash: "seedhash", Name: "seeded scene", Category: "forager",
		State: "uploading", Progress: 1, ContentPath: "/downloads/seeded",
		Ratio: ratio, SeedingTime: seedingSecs,
		CompletionOn: time.Now().Add(-30 * 24 * time.Hour).Unix(),
	}
}

// TestCullOnRatio: the ratio rule fires first — a torrent at 1.0 with only
// a day of seeding retires, files deleted (the library hardlink is the
// survivor), and the deletion is journalled.
func TestCullOnRatio(t *testing.T) {
	r, _, placed := cullRig(t)
	r.qbit.set([]qbit.Torrent{seededTorrent(1.2, int64((24 * time.Hour).Seconds()))})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 1 || got[0] != "seedhash:true" {
		t.Fatalf("deletes = %v, want [seedhash:true]", got)
	}
	if _, err := os.Stat(placed); err != nil {
		t.Fatalf("library copy must be untouched: %v", err)
	}
	entries, err := r.repo.ListDestructions(context.Background(), 5)
	if err != nil || len(entries) != 1 || entries[0].Reason != "seeding cull" {
		t.Fatalf("journal = %+v (%v), want one seeding-cull entry", entries, err)
	}
}

// TestCullOnAge: the age rule fires when the ratio never will — 8 days
// seeded at ratio 0.3 retires.
func TestCullOnAge(t *testing.T) {
	r, _, _ := cullRig(t)
	r.qbit.set([]qbit.Torrent{seededTorrent(0.3, int64((8 * 24 * time.Hour).Seconds()))})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 1 {
		t.Fatalf("deletes = %v, want the age rule to fire", got)
	}
}

// TestCullNeitherThresholdKeepsSeeding: below both thresholds, nothing
// happens — the whole point of "whichever first" is that neither alone has
// been earned yet.
func TestCullNeitherThresholdKeepsSeeding(t *testing.T) {
	r, _, _ := cullRig(t)
	r.qbit.set([]qbit.Torrent{seededTorrent(0.4, int64((2 * 24 * time.Hour).Seconds()))})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none", got)
	}
}

// TestCullNeverTouchesUntrackedTorrents is the ownership guard: a torrent
// parked in the forage category with no grab row is NOT forage's to
// delete, whatever its ratio.
func TestCullNeverTouchesUntrackedTorrents(t *testing.T) {
	r, _, _ := cullRig(t)
	stranger := seededTorrent(9.9, int64((99 * 24 * time.Hour).Seconds()))
	stranger.Hash = "not-ours"
	stranger.Name = "someone else's torrent"
	r.qbit.set([]qbit.Torrent{stranger})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 0 {
		t.Fatalf("deletes = %v — an untracked torrent must never be culled", got)
	}
}

// TestCullRefusesWhenLibraryCopyMissing is the data-loss guard: if the
// placed file can't be stat'd, the client copy may be the ONLY copy, and
// deleting it would be loss, not cleanup.
func TestCullRefusesWhenLibraryCopyMissing(t *testing.T) {
	r, _, placed := cullRig(t)
	if err := os.Remove(placed); err != nil {
		t.Fatal(err)
	}
	r.qbit.set([]qbit.Torrent{seededTorrent(2.0, int64((30 * 24 * time.Hour).Seconds()))})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 0 {
		t.Fatalf("deletes = %v — a torrent whose library copy is missing must be kept", got)
	}
}

// TestCullIgnoresIncompleteTorrents: progress < 1 can never cull, whatever
// the clock says — CompletionOn/SeedingTime on an incomplete torrent are
// leftovers, not evidence.
func TestCullIgnoresIncompleteTorrents(t *testing.T) {
	r, _, _ := cullRig(t)
	tor := seededTorrent(2.0, int64((30 * 24 * time.Hour).Seconds()))
	tor.Progress = 0.8
	r.qbit.set([]qbit.Torrent{tor})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none for an incomplete torrent", got)
	}
}

// TestCullDisabledWhenBothZero: both thresholds zero = feature off, and
// the pass must not even list torrents (no client load).
func TestCullDisabledWhenBothZero(t *testing.T) {
	r, _, _ := cullRig(t)
	r.setConfig(func(c *config.Config) {
		c.SeedMaxAge = 0
		c.SeedRatio = 0
	})
	r.qbit.set([]qbit.Torrent{seededTorrent(9.9, int64((99 * 24 * time.Hour).Seconds()))})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none when disabled", got)
	}
}

// TestCullPassCap: a deep backlog retires gradually — cullPassCap per pass,
// so the first run against years of seeding is observable, not a stampede.
func TestCullPassCap(t *testing.T) {
	r, _, placed := cullRig(t)
	ctx := context.Background()
	var ts []qbit.Torrent
	for i := 0; i < cullPassCap+10; i++ {
		tor := seededTorrent(2.0, int64((30 * 24 * time.Hour).Seconds()))
		tor.Hash = tor.Hash + string(rune('a'+i%26)) + string(rune('a'+i/26))
		ts = append(ts, tor)
		if _, err := r.repo.Insert(ctx, grabs.Grab{
			ReleaseTitle: "bulk", Client: "qbit", ClientID: tor.Hash,
			Category: "forager", Status: "confirmed", PlacedPath: placed,
			Kind: "single", GrabbedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	r.qbit.set(ts)

	r.poller.cullSeededTorrents(ctx)

	if got := r.qbit.deletedCalls(); len(got) != cullPassCap {
		t.Fatalf("deletes = %d, want the cap (%d)", len(got), cullPassCap)
	}
}

// TestCullPerIndexerOverride: a PornoLab override at ratio 2.0 / 30 days
// must protect a PornoLab torrent that the GLOBAL thresholds would retire,
// while an identical torrent from a non-overridden indexer retires as
// usual. Matching is case-insensitive (Prowlarr's spelling varies).
func TestCullPerIndexerOverride(t *testing.T) {
	r, _, placed := cullRig(t)
	ctx := context.Background()
	r.setConfig(func(c *config.Config) {
		c.SeedOverrides = `[{"indexer":"PornoLab","maxAge":"720h","ratio":2.0}]`
	})

	protected := seededTorrent(1.2, int64((10 * 24 * time.Hour).Seconds()))
	protected.Hash = "protectedhash"
	plain := seededTorrent(1.2, int64((10 * 24 * time.Hour).Seconds()))
	plain.Hash = "plainhash"
	r.qbit.set([]qbit.Torrent{protected, plain})
	for _, row := range []struct{ hash, indexer string }{
		{"protectedhash", "pornolab"}, // deliberately lowercased vs the override
		{"plainhash", "NZBgeek"},
	} {
		if _, err := r.repo.Insert(ctx, grabs.Grab{
			ReleaseTitle: "t-" + row.hash, Client: "qbit", ClientID: row.hash,
			ReleaseIndexer: row.indexer, Category: "forager",
			Status: "confirmed", PlacedPath: placed, Kind: "single", GrabbedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	r.poller.cullSeededTorrents(ctx)

	got := r.qbit.deletedCalls()
	if len(got) != 1 || got[0] != "plainhash:true" {
		t.Fatalf("deletes = %v, want only the non-overridden torrent", got)
	}
}

// TestCullOverrideExplicitZeroDisablesRule: {"maxAge":"0"} means "seed this
// tracker's torrents forever unless the ratio rule fires" — an old
// low-ratio torrent survives, but the same tracker's ratio-met torrent
// still retires (the ratio field was omitted, so it inherits the global).
func TestCullOverrideExplicitZeroDisablesRule(t *testing.T) {
	r, _, placed := cullRig(t)
	ctx := context.Background()
	r.setConfig(func(c *config.Config) {
		c.SeedOverrides = `[{"indexer":"empornium","maxAge":"0"}]`
	})

	oldLowRatio := seededTorrent(0.2, int64((90 * 24 * time.Hour).Seconds()))
	oldLowRatio.Hash = "oldlow"
	ratioMet := seededTorrent(1.5, int64((1 * 24 * time.Hour).Seconds()))
	ratioMet.Hash = "ratiomet"
	r.qbit.set([]qbit.Torrent{oldLowRatio, ratioMet})
	for _, h := range []string{"oldlow", "ratiomet"} {
		if _, err := r.repo.Insert(ctx, grabs.Grab{
			ReleaseTitle: "t-" + h, Client: "qbit", ClientID: h,
			ReleaseIndexer: "empornium", Category: "forager",
			Status: "confirmed", PlacedPath: placed, Kind: "single", GrabbedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	r.poller.cullSeededTorrents(ctx)

	got := r.qbit.deletedCalls()
	if len(got) != 1 || got[0] != "ratiomet:true" {
		t.Fatalf("deletes = %v, want only the ratio-met torrent (age rule disabled)", got)
	}
}

// TestCullMalformedOverridesPausesEverything is the safety direction:
// overrides exist to PROTECT trackers, so a typo must pause the whole cull
// — falling back to globals would retire exactly the torrents the user was
// trying to shield.
func TestCullMalformedOverridesPausesEverything(t *testing.T) {
	r, _, _ := cullRig(t)
	r.setConfig(func(c *config.Config) {
		c.SeedOverrides = `[{"indexer":"PornoLab","maxAge":"one week"}]` // not a duration
	})
	r.qbit.set([]qbit.Torrent{seededTorrent(9.9, int64((99 * 24 * time.Hour).Seconds()))})

	r.poller.cullSeededTorrents(context.Background())

	if got := r.qbit.deletedCalls(); len(got) != 0 {
		t.Fatalf("deletes = %v — malformed overrides must pause the cull entirely", got)
	}
}
