package api

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/ordureconnoisseur/forager/internal/cache"
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
