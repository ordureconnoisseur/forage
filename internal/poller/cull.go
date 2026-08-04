package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/destroy"
	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/seeding"
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

// seedOverride is one per-indexer threshold entry from the SeedOverrides
// JSON. Pointer fields distinguish "omitted → inherit the global" from an
// explicit zero, which disables that rule for the indexer — the natural
// spelling for "seed my private-tracker torrents forever".
type seedOverride struct {
	Indexer string   `json:"indexer"`
	MaxAge  *string  `json:"maxAge,omitempty"` // Go duration string, e.g. "720h"
	Ratio   *float64 `json:"ratio,omitempty"`
}

// cullThresholds are the resolved (global or overridden) limits for one
// torrent.
type cullThresholds struct {
	maxAge time.Duration
	ratio  float64
}

// parseSeedOverrides builds the case-insensitive indexer → thresholds map.
// An error here PAUSES the cull rather than falling back to globals: the
// whole point of an override is to PROTECT specific trackers' torrents, and
// culling exactly those because of a typo in the config would be the worst
// possible reading of it.
func parseSeedOverrides(raw string, global cullThresholds) (map[string]cullThresholds, error) {
	out := map[string]cullThresholds{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	var entries []seedOverride
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("seedOverrides is not valid JSON: %w", err)
	}
	for i, e := range entries {
		name := strings.ToLower(strings.TrimSpace(e.Indexer))
		if name == "" {
			return nil, fmt.Errorf("seedOverrides[%d] has no indexer name", i)
		}
		th := global
		if e.MaxAge != nil {
			d, err := time.ParseDuration(*e.MaxAge)
			if err != nil || d < 0 {
				return nil, fmt.Errorf("seedOverrides[%d] (%s): bad maxAge %q", i, e.Indexer, *e.MaxAge)
			}
			th.maxAge = d
		}
		if e.Ratio != nil {
			if *e.Ratio < 0 {
				return nil, fmt.Errorf("seedOverrides[%d] (%s): negative ratio", i, e.Indexer)
			}
			th.ratio = *e.Ratio
		}
		out[name] = th
	}
	return out, nil
}

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
	qb := p.pool.Torrents()
	if qb == nil {
		return
	}
	torrents, err := qb.ListTorrents(ctx, qbit.ListOpts{Filter: "completed", Category: cfg.QbitCategory})
	if err != nil {
		p.log.Warn("seeding cull: list torrents", "err", err)
		return
	}

	global := cullThresholds{maxAge: cfg.SeedMaxAge, ratio: cfg.SeedRatio}
	overrides, oerr := parseSeedOverrides(cfg.SeedOverrides, global)
	if oerr != nil {
		// Pause, don't fall back: overrides exist to PROTECT trackers, and
		// globals would cull exactly the torrents the user tried to shield.
		p.log.Error("seeding cull PAUSED: fix seedOverrides", "err", oerr)
		return
	}

	// EVERY torrent, not just forage's: the cull deletes files, and the
	// thing it must not do is delete a file some other torrent is serving.
	// A failure here skips the pass rather than culling blind, because the
	// whole point of this list is to say no.
	allTorrents, aerr := qb.ListTorrents(ctx, qbit.ListOpts{})
	if aerr != nil {
		p.log.Warn("seeding cull: cannot list all torrents, skipping pass", "err", aerr)
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
		if t.Progress < 1 {
			continue
		}
		// Ownership before verdict: the thresholds can depend on the grab's
		// indexer, so the lookup comes first. Sub-millisecond against the
		// local DB; the hourly pass over a few hundred rows doesn't notice.
		g, gerr := p.repo.ByClientID(ctx, "qbit", t.Hash)
		if gerr != nil {
			p.log.Warn("seeding cull: grab lookup", "hash", t.Hash, "err", gerr)
			continue
		}
		if g == nil {
			continue // not forage's torrent — never touch it
		}
		th := global
		if o, ok := overrides[strings.ToLower(strings.TrimSpace(g.ReleaseIndexer))]; ok && g.ReleaseIndexer != "" {
			th = o
		}
		due, why := cullDue(t, th.maxAge, th.ratio, now)
		if !due {
			continue
		}
		if g.PlacedPath == "" {
			continue // never placed: the client copy may be the only copy
		}
		// A placement the heal failed to remove is a PARTIAL that happens to
		// exist. Stat'ing it would "verify" a library copy that was never
		// complete, and the cull would then delete the torrent holding the
		// only whole file.
		if removalPending(g) {
			p.log.Warn("seeding cull: placement pending removal, keeping torrent",
				"id", g.ID, "path", g.PlacedPath)
			continue
		}
		if _, serr := os.Stat(g.PlacedPath); serr != nil {
			p.log.Warn("seeding cull: placed copy not verifiable, keeping torrent",
				"id", g.ID, "path", g.PlacedPath, "err", serr)
			continue
		}
		// Another torrent serving the same bytes makes this NOT redundant.
		//
		// Two grabs of one release download to the same filename, so both
		// torrents point at a single file. Culling either one deletes the
		// file the other is seeding, which is how a torrent on this library
		// sat in missingFiles for a fortnight: the June grab was retired on
		// schedule and took the July grab's file with it. The library copy
		// was safe throughout, so nothing was lost, but a torrent died
		// silently and nothing said so.
		if other := otherSeeder(allTorrents, t.Hash, t.ContentPath); other != "" {
			p.log.Warn("seeding cull: another torrent is serving this path, keeping it",
				"id", g.ID, "path", t.ContentPath, "also_seeded_by", other)
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

// otherSeeder returns the content path of a torrent OTHER than selfHash that
// is serving path, or "" when none is.
//
// Uses seeding.Blocks rather than a string compare so it also catches the
// folder cases: a pack directory whose contents another torrent serves, and a
// file inside a directory another torrent holds.
func otherSeeder(all []qbit.Torrent, selfHash, path string) string {
	entries := make([]seeding.Entry, 0, len(all))
	for _, o := range all {
		entries = append(entries, seeding.Entry{ID: o.Hash, Path: o.ContentPath})
	}
	return seeding.NewFrom(entries, selfHash, seeding.DefaultMinDepth).Blocks(path)
}
