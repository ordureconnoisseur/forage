package poller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/destroy"
	"github.com/ordureconnoisseur/forager/internal/grabs"
	"github.com/ordureconnoisseur/forager/internal/qbit"
)

// Retiring downloads that are never going to finish.
//
// The seeding cull deliberately returns early for anything incomplete, so a
// torrent that stops mid-download is invisible to it forever. On the reference
// library that left 43 of them holding 339.6 GB of partial data, 11 added over
// a month earlier, every one correctly badged "stalled" in the UI and acted on
// by nothing.
//
// The badge already exists and is the wrong tool: stalledAfter is 30 minutes,
// which answers "is this stuck right now" for someone looking at the screen.
// Retirement is a different question and needs a threshold long enough that a
// torrent starved of peers for a month is genuinely dead rather than unlucky.
//
// Reports by default and removes only when told, because deleting a partial
// download is unrecoverable and the pass runs unattended.

// DeadActionRemove is the DeadDownloads setting that actually retires them.
// Anything else (including the default) reports and touches nothing.
const DeadActionRemove = "remove"

// deadDue reports whether an unfinished torrent has been idle long enough to
// count as dead, and why.
//
// Idleness is measured from the grab's progress_at, the moment the download
// last moved a byte, NOT from when it was added. A torrent inching along at a
// few KB/s keeps stamping progress and is never dead however old it is; one
// that has not moved in a month is, however recently it was queued.
func deadDue(t qbit.Torrent, g *grabs.Grab, after time.Duration, now time.Time) (bool, string) {
	if after <= 0 || g == nil {
		return false, ""
	}
	if t.Progress >= 1 {
		return false, "" // finished: the seeding cull owns it
	}
	// Metadata fetching is a different failure with no content on disk yet,
	// and a magnet that takes a while to resolve must not be swept up by a
	// rule about downloads that stopped.
	if strings.EqualFold(t.State, "metaDL") {
		return false, ""
	}
	// progress_at is stamped whenever the download advances. Falling back to
	// the grab time covers a torrent that never moved at all, which is the
	// deadest case of the lot.
	since := g.ProgressAt
	if since == 0 {
		since = g.GrabbedAt
	}
	if since == 0 {
		return false, "" // no clock to judge by: leave it alone
	}
	idle := now.Sub(time.Unix(since, 0))
	if idle < after {
		return false, ""
	}
	return true, fmt.Sprintf("no progress for %s (past %s), %.1f%% complete",
		idle.Truncate(time.Hour), after, t.Progress*100)
}

// cullDeadDownloads runs one pass over unfinished torrents.
func (p *Poller) cullDeadDownloads(ctx context.Context) {
	cfg := p.pool.Settings()
	if cfg.DeadAfter <= 0 {
		return
	}
	qb := p.pool.Torrents()
	if qb == nil {
		return
	}
	remove := strings.EqualFold(strings.TrimSpace(cfg.DeadDownloads), DeadActionRemove)

	torrents, err := qb.ListTorrents(ctx, qbit.ListOpts{Category: cfg.QbitCategory})
	if err != nil {
		p.log.Warn("dead-download cull: list torrents", "err", err)
		return
	}
	var all []qbit.Torrent
	if remove {
		// Only needed to refuse collateral, so it is not fetched when the
		// pass cannot delete anything anyway.
		all, err = qb.ListTorrents(ctx, qbit.ListOpts{})
		if err != nil {
			p.log.Warn("dead-download cull: cannot list all torrents, reporting only", "err", err)
			remove = false
		}
	}

	now := time.Now()
	found, removed := 0, 0
	for _, t := range torrents {
		g, gerr := p.repo.ByClientID(ctx, "qbit", t.Hash)
		if gerr != nil || g == nil {
			continue // not forage's torrent, or unreadable: never touch it
		}
		due, why := deadDue(t, g, cfg.DeadAfter, now)
		if !due {
			continue
		}
		found++
		if !remove {
			p.log.Info("dead download (reporting only)", "id", g.ID,
				"name", t.Name, "why", why, "progress", t.Progress)
			continue
		}
		if other := otherSeeder(all, t.Hash, t.ContentPath); other != "" {
			p.log.Warn("dead download: another torrent serves this path, keeping it",
				"id", g.ID, "path", t.ContentPath, "also_seeded_by", other)
			continue
		}
		if _, jerr := p.repo.JournalDestruction(ctx, "dead download cull",
			destroy.Target{Files: []destroy.File{{Path: t.ContentPath}}},
			"destroyed", why); jerr != nil {
			p.log.Warn("dead-download cull: journal", "id", g.ID, "err", jerr)
		}
		if derr := qb.DeleteTorrent(ctx, t.Hash, true); derr != nil {
			p.log.Warn("dead-download cull: delete torrent", "id", g.ID, "err", derr)
			continue
		}
		removed++
		p.log.Info("dead download retired", "id", g.ID, "name", t.Name, "why", why)
	}
	if found > 0 {
		p.log.Info("dead-download cull pass done",
			"found", found, "removed", removed, "action", cfg.DeadDownloads)
	}
}
