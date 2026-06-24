package cache

import (
	"context"

	"github.com/ordureconnoisseur/forager/internal/stash"
)

// OwnedSceneCounts computes per-subject owned-scene counts from the LOCAL
// library, keyed by StashDB cross-id — the lazy cache redesign's owned-count
// source (no StashDB queries). Counts are deduped by scene cross-id so multiple
// local copies of one scene count once, and a scene listing a performer twice
// counts that performer once. Returns (performerCounts, studioCounts), each
// map[stashDBCrossID]int.
//
// This mirrors the semantics of the current eager method (which counts distinct
// owned StashDB scene ids that fall under a subject) but derives attribution
// from the local scene's own studio/performer cross-ids instead of downloading
// the subject's StashDB catalogue.
func OwnedSceneCounts(ctx context.Context, sc *stash.Client) (performers, studios map[string]int, err error) {
	attrs, err := sc.FindOwnedSceneAttribution(ctx)
	if err != nil {
		return nil, nil, err
	}
	performers = map[string]int{}
	studios = map[string]int{}
	seenScene := make(map[string]bool, len(attrs))
	for _, a := range attrs {
		if seenScene[a.SceneStashDBID] {
			continue // dedup multiple local copies of the same StashDB scene
		}
		seenScene[a.SceneStashDBID] = true
		if a.StudioStashDBID != "" {
			studios[a.StudioStashDBID]++
		}
		seenPerf := map[string]bool{}
		for _, p := range a.PerformerStashDBIDs {
			if seenPerf[p] {
				continue
			}
			seenPerf[p] = true
			performers[p]++
		}
	}
	return performers, studios, nil
}
