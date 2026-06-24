package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/ordureconnoisseur/forager/internal/cache"
	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// getOwnedCountCheck is a SHADOW diagnostic for the lazy cache redesign: it
// computes per-subject owned-scene counts the new way (locally, via
// cache.OwnedSceneCounts) and diffs them against the live owned_scenes_count
// the eager pass populated. Proves the new attribution matches before anything
// switches over. Read-only; changes nothing.
func (s *Server) getOwnedCountCheck(w http.ResponseWriter, r *http.Request) {
	sc := s.pool.Stash()
	if sc == nil {
		writeErr(w, http.StatusServiceUnavailable, "stash not configured")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	perfNew, studNew, err := cache.OwnedSceneCounts(ctx, sc)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "owned sweep: "+err.Error())
		return
	}

	type sample struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Old   int    `json:"old"`
		New   int    `json:"new"`
		Total int    `json:"total"`
		AtCap bool   `json:"at_cap"`
	}
	type summary struct {
		Total           int      `json:"total"`
		Matched         int      `json:"matched"`
		Mismatched      int      `json:"mismatched"`
		MismatchedAtCap int      `json:"mismatched_at_cap"`
		Samples         []sample `json:"samples"`
	}

	check := func(table string, newCounts map[string]int) (summary, error) {
		rows, err := s.db.QueryContext(ctx,
			`SELECT stashdb_id, COALESCE(name,''), owned_scenes_count, total_stashdb_scenes
			   FROM `+table+`
			  WHERE stashdb_id IS NOT NULL AND stashdb_id != '' AND stashdb_id NOT LIKE 'stash:%'`)
		if err != nil {
			return summary{}, err
		}
		defer rows.Close()
		var sum summary
		var diffs []sample
		for rows.Next() {
			var id, name string
			var old, total int
			if err := rows.Scan(&id, &name, &old, &total); err != nil {
				return summary{}, err
			}
			sum.Total++
			nw := newCounts[id]
			if old == nw {
				sum.Matched++
				continue
			}
			sum.Mismatched++
			atCap := total >= 5000 // eager pass capped this subject → expected drift
			if atCap {
				sum.MismatchedAtCap++
			}
			diffs = append(diffs, sample{ID: id, Name: name, Old: old, New: nw, Total: total, AtCap: atCap})
		}
		if err := rows.Err(); err != nil {
			return summary{}, err
		}
		// Biggest discrepancies first; keep a sample.
		sort.Slice(diffs, func(i, j int) bool {
			return abs(diffs[i].Old-diffs[i].New) > abs(diffs[j].Old-diffs[j].New)
		})
		if len(diffs) > 25 {
			diffs = diffs[:25]
		}
		sum.Samples = diffs
		return sum, nil
	}

	perfSum, err := check("performer_cache", perfNew)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "performer diff: "+err.Error())
		return
	}
	studSum, err := check("studio_cache", studNew)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "studio diff: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"performers": perfSum,
		"studios":    studSum,
	})
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// getIDCountCheck verifies the ID-ONLY count path: for a sample of subjects it
// fetches just the scene ids from StashDB (the lightweight count query),
// computes total + owned (ids ∩ owned set), and diffs against the live cache
// numbers. Unlike the local-tag method, this uses the same
// StashDB-scene-ids-∩-owned semantics as the eager pass, so it SHOULD match
// exactly — proving the lighter count path preserves the bars before we wire it.
func (s *Server) getIDCountCheck(w http.ResponseWriter, r *http.Request) {
	sc := s.pool.Stash()
	sdb := s.pool.StashDB()
	if sc == nil || sdb == nil {
		writeErr(w, http.StatusServiceUnavailable, "stash and stashdb required")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	ownedIDs, err := sc.FindAllOwnedStashDBSceneIDs(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "owned sweep: "+err.Error())
		return
	}
	owned := make(map[string]bool, len(ownedIDs))
	for _, id := range ownedIDs {
		owned[id] = true
	}

	type sample struct {
		Name       string `json:"name"`
		OldTotal   int    `json:"old_total"`
		NewTotal   int    `json:"new_total"`
		OldOwned   int    `json:"old_owned"`
		NewOwned   int    `json:"new_owned"`
		OwnedMatch bool   `json:"owned_match"`
	}
	check := func(table string, isStudio bool) ([]sample, int, int) {
		rows, err := s.db.QueryContext(ctx, `
			SELECT stashdb_id, COALESCE(name,''), owned_scenes_count, total_stashdb_scenes
			  FROM `+table+`
			 WHERE stashdb_id IS NOT NULL AND stashdb_id != '' AND stashdb_id NOT LIKE 'stash:%'
			 ORDER BY owned_scenes_count DESC LIMIT 20`)
		if err != nil {
			return nil, 0, 0
		}
		defer rows.Close()
		var out []sample
		matched := 0
		for rows.Next() {
			var id, name string
			var oldOwned, oldTotal int
			if err := rows.Scan(&id, &name, &oldOwned, &oldTotal); err != nil {
				continue
			}
			q := stashdb.SceneQuery{}
			if isStudio {
				q.StudioIDs = []string{id}
			} else {
				q.PerformerIDs = []string{id}
			}
			cnt, err := sdb.QuerySceneIDs(ctx, q, 5000)
			if err != nil {
				continue
			}
			newOwned := 0
			for _, sid := range cnt.IDs {
				if owned[sid] {
					newOwned++
				}
			}
			ok := newOwned == oldOwned
			if ok {
				matched++
			}
			out = append(out, sample{Name: name, OldTotal: oldTotal, NewTotal: cnt.Total, OldOwned: oldOwned, NewOwned: newOwned, OwnedMatch: ok})
		}
		return out, matched, len(out)
	}

	perf, pm, pn := check("performer_cache", false)
	stud, sm, sn := check("studio_cache", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"performers": map[string]any{"sampled": pn, "owned_matched": pm, "samples": perf},
		"studios":    map[string]any{"sampled": sn, "owned_matched": sm, "samples": stud},
	})
}
