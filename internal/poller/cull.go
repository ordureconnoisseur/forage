package poller

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ordureconnoisseur/forager/internal/destroy"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// ── Seeding cull ────────────────────────────────────────────────────
//
// Hardlink placement means a completed torrent's files exist twice by
// design: the client's copy (seeding) and the library's (Stash's). The
// second link is free — until the torrent is removed, at which point the
// client copy was the only thing pinning the download client's disk.
// Nothing ever removed it, so the client accumulated every torrent forage
// ever grabbed.
//
// The cull retires a torrent once it has EARNED retirement — a seeding
// ratio or a seeding age, whichever is met first (defaults 1.0 / 7 days,
// which clears the common private-tracker hit-and-run rules). Removal
// deletes the client's files too; the library hardlink is untouched, so
// the media survives — but only if it actually exists, which is why the
// guards below are the feature:
//
//   - forage only culls torrents IT TRACKS (a grab row for the hash). A
//     torrent someone parked in the forage category is never forage's to
//     delete.
//   - the grab must be PLACED and its placed path must stat right now.
//     No placement, or a vanished file, means the client copy may be the
//     ONLY copy — culling it would be data loss, not cleanup.
//   - the library-health latch applies: during a mount outage every stat
//     would fail as "vanished", so the cull sits the tick out entirely.
//
// Every cull is journalled (the client copy is a real deletion, even
// though the library copy survives), and each pass is capped so the first
// run against a years-deep seed list retires it gradually and observably
// rather than in one thousand-torrent stampede.

// cullPassCap bounds deletions per pass; with an hourly cadence a deep
// backlog drains in days while staying easy to watch in the journal.
const cullPassCap = 25

// cullInterval is how often the pass runs. Ratios and ages move slowly;
// hourly is prompt without adding client load worth noticing.
const cullInterval = time.Hour

// cullDue reports whether a completed torrent has met either retirement
// threshold, and which. Zero-valued thresholds disable their rule.
func cullDue(t qbit.Torrent, maxAge time.Duration, ratio float64, now time.Time) (bool, string) {
	if t.Progress < 1 {
		return false, ""
	}
	if ratio > 0 && t.Ratio >= ratio {
		return true, fmt.Sprintf("ratio %.2f reached %.2f", t.Ratio, ratio)
	}
	if maxAge > 0 {
		// qBit's own active-seeding counter when present (doesn't count
		// client downtime); completion-clock fallback otherwise.
		var seedFor time.Duration
		if t.SeedingTime > 0 {
			seedFor = time.Duration(t.SeedingTime) * time.Second
		} else if t.CompletionOn > 0 {
			seedFor = now.Sub(time.Unix(t.CompletionOn, 0))
		}
		if seedFor >= maxAge {
			return true, fmt.Sprintf("seeded %s, past %s", seedFor.Truncate(time.Minute), maxAge)
		}
	}
	return false, ""
}

// cullSeededTorrents runs one capped cull pass. Best-effort: any lookup or
// delete failure skips that torrent and the next pass retries.
func (p *Poller) cullSeededTorrents(ctx context.Context) {
	cfg := p.pool.Settings()
	if cfg.SeedMaxAge <= 0 && cfg.SeedRatio <= 0 {
		return
	}
	if !p.libraryHealthy() {
		p.log.Warn("seeding cull skipped: library mount unavailable")
		return
	}
	qb := p.pool.Qbit()
	if qb == nil {
		return
	}
	torrents, err := qb.ListTorrents(ctx, qbit.ListOpts{Filter: "completed", Category: cfg.QbitCategory})
	if err != nil {
		p.log.Warn("seeding cull: list torrents", "err", err)
		return
	}

	now := time.Now()
	culled := 0
	for _, t := range torrents {
		if culled >= cullPassCap {
			p.log.Info("seeding cull: pass cap reached, more next pass",
				"cap", cullPassCap, "candidates_remaining", len(torrents))
			break
		}
		due, why := cullDue(t, cfg.SeedMaxAge, cfg.SeedRatio, now)
		if !due {
			continue
		}
		g, gerr := p.repo.ByClientID(ctx, "qbit", t.Hash)
		if gerr != nil {
			p.log.Warn("seeding cull: grab lookup", "hash", t.Hash, "err", gerr)
			continue
		}
		if g == nil {
			continue // not forage's torrent — never touch it
		}
		if g.PlacedPath == "" {
			continue // never placed: the client copy may be the only copy
		}
		if _, serr := os.Stat(g.PlacedPath); serr != nil {
			p.log.Warn("seeding cull: placed copy not verifiable, keeping torrent",
				"id", g.ID, "path", g.PlacedPath, "err", serr)
			continue
		}
		// The library copy is verified on disk; the client copy is now
		// genuinely redundant. Journal, then delete torrent + files.
		if _, jerr := p.repo.JournalDestruction(ctx, "seeding cull",
			destroy.Target{Files: []destroy.File{{Path: t.ContentPath}}},
			"destroyed", why+" (library copy verified at "+g.PlacedPath+")"); jerr != nil {
			p.log.Warn("seeding cull: journal", "id", g.ID, "err", jerr)
		}
		if derr := qb.DeleteTorrent(ctx, t.Hash, true); derr != nil {
			p.log.Warn("seeding cull: delete torrent", "id", g.ID, "hash", t.Hash, "err", derr)
			continue
		}
		culled++
		p.log.Info("seeding cull: retired torrent", "id", g.ID,
			"name", t.Name, "why", why, "ratio", t.Ratio)
	}
	if culled > 0 {
		p.log.Info("seeding cull pass done", "culled", culled, "scanned", len(torrents))
	}
}
