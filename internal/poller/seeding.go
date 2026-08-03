package poller

import (
	"context"

	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/seeding"
)

// seedingSet snapshots the paths qBittorrent is currently serving, so the
// automatic pack dedup cannot delete a file a torrent is reading from.
//
// This surface matters more than the UI ones: nobody is looking at it. A
// duplicate the poller removes on its own is exactly the case where a broken
// torrent goes unnoticed until the ratio is already gone.
//
// Returns nil with no torrent client or when qBit is unreachable, which
// destroy.VetWith reads as "no information" and which therefore refuses
// nothing extra. A transient qBit failure must not stall the pipeline.
func (p *Poller) seedingSet(ctx context.Context) *seeding.Set {
	qb := p.pool.Torrents()
	if qb == nil {
		return nil
	}
	torrents, err := qb.ListTorrents(ctx, qbit.ListOpts{})
	if err != nil {
		p.log.Warn("seeding snapshot failed; dedup cannot protect seeded files", "err", err)
		return nil
	}
	paths := make([]string, 0, len(torrents))
	for _, t := range torrents {
		paths = append(paths, t.ContentPath)
	}
	return seeding.New(paths, seeding.DefaultMinDepth)
}
