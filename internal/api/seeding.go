package api

import (
	"context"

	"github.com/ordureconnoisseur/forager/internal/qbit"
	"github.com/ordureconnoisseur/forager/internal/seeding"
)

// seedingSet snapshots the paths qBittorrent is currently serving, for the
// destroy invariant that refuses to remove a file out from under a torrent.
//
// One call per plan, not per file: a duplicate sweep can vet thousands of
// targets, and the answer must not change halfway through a plan the user has
// already been shown a preview of.
//
// Returns nil when there is no torrent client, or when qBit cannot be reached.
// destroy.VetWith treats that as "no information" and refuses nothing extra,
// so an outage degrades to the previous behaviour rather than freezing every
// delete surface in forage.
func (s *Server) seedingSet(ctx context.Context) *seeding.Set {
	qb := s.pool.Torrents()
	if qb == nil {
		return nil
	}
	torrents, err := qb.ListTorrents(ctx, qbit.ListOpts{})
	if err != nil {
		s.log.Warn("seeding snapshot failed; destroy plans cannot protect seeded files",
			"err", err)
		return nil
	}
	paths := make([]string, 0, len(torrents))
	for _, t := range torrents {
		paths = append(paths, t.ContentPath)
	}
	set := seeding.New(paths, seeding.DefaultMinDepth)
	s.log.Debug("seeding snapshot", "torrents", len(torrents), "usable_paths", set.Len())
	return set
}
