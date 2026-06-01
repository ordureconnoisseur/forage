// Package suggest maps a torrent/release name to the local performer
// whose library folder it most likely belongs in. Shared by the manual
// torrent-add API (inspect → suggested folder) and the poller's
// qBit-adoption path, so a download that arrives by either route folds
// into the same performer directory.
package suggest

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/ordureconnoisseur/forager/internal/matcher"
)

// Performer is a ranked folder suggestion for a release name.
type Performer struct {
	StashID    string
	Name       string
	SceneCount int
	Favorite   bool
	// MatchLen is how many whole words of this performer's name/alias
	// matched in the release name. Higher = more specific (a full
	// "First Last" match is 2; a lone first name is 1). Callers that must
	// avoid false positives (auto-adoption) gate on this; the interactive
	// picker shows all candidates regardless.
	MatchLen int
}

// Performers ranks local performers whose name (or an alias) appears as a
// contiguous whole-word phrase in name — e.g. "Amouranth" or "<Studio>
// Hazel Moore SiteRip" → that performer. Matching is word-level (not
// substring) so "Mom" can't match "momdrips", and a single-word label
// must be >= 4 chars to skip short common words. Best effort: a query
// failure yields nil. Order: most-specific (longest matched phrase)
// first, then favourites, then library prominence (scene_count), then
// name for stability.
func Performers(ctx context.Context, db *sql.DB, name string) []Performer {
	nameWords := matcher.Tokenize(name)
	if len(nameWords) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT stash_id, name, aliases, scene_count, favorite FROM performer_cache`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type cand struct {
		p        Performer
		matchLen int // matched token count — higher is more specific
	}
	var cands []cand
	for rows.Next() {
		var stashID, pname string
		var aliases sql.NullString
		var sceneCount, fav int
		if rows.Scan(&stashID, &pname, &aliases, &sceneCount, &fav) != nil {
			continue
		}
		best := 0
		labels := append([]string{pname}, parseAliasList(aliases.String)...)
		for _, label := range labels {
			lw := matcher.Tokenize(label)
			if labelMatches(nameWords, lw) && len(lw) > best {
				best = len(lw)
			}
		}
		if best > 0 {
			cands = append(cands, cand{
				p:        Performer{StashID: stashID, Name: pname, SceneCount: sceneCount, Favorite: fav != 0},
				matchLen: best,
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.matchLen != b.matchLen {
			return a.matchLen > b.matchLen
		}
		if a.p.Favorite != b.p.Favorite {
			return a.p.Favorite
		}
		if a.p.SceneCount != b.p.SceneCount {
			return a.p.SceneCount > b.p.SceneCount
		}
		return a.p.Name < b.p.Name
	})
	out := make([]Performer, 0, len(cands))
	for _, c := range cands {
		p := c.p
		p.MatchLen = c.matchLen
		out = append(out, p)
	}
	return out
}

// TopFolder returns the single best performer-folder name for a release,
// or "" when nothing matches.
func TopFolder(ctx context.Context, db *sql.DB, name string) string {
	ps := Performers(ctx, db, name)
	if len(ps) == 0 {
		return ""
	}
	return ps[0].Name
}

// ConfidentTopFolder is TopFolder for auto-adoption: it only returns a name
// when the best match is a full multi-word performer name (MatchLen >= 2),
// AND it's unambiguous (no second candidate matched the same number of
// words — which would mean two performers both fit, e.g. co-stars or a
// shared first name). Otherwise it returns "" so the caller leaves the
// grab Unsorted rather than committing a likely-wrong folder. A lone
// first-name / single-word hit (MatchLen 1) is exactly the false-positive
// class that mis-filed adopted torrents, so single-word matches never
// auto-adopt — even though the interactive picker still surfaces them.
func ConfidentTopFolder(ctx context.Context, db *sql.DB, name string) string {
	ps := Performers(ctx, db, name)
	if len(ps) == 0 || ps[0].MatchLen < 2 {
		return ""
	}
	// Ambiguous when a second candidate matched just as many words — two
	// full names both fit the release, so we can't pick one safely.
	if len(ps) > 1 && ps[1].MatchLen == ps[0].MatchLen {
		return ""
	}
	return ps[0].Name
}

// labelMatches reports whether labelWords appears as a contiguous run in
// nameWords. A single-word label must be >= 4 chars so short common words
// don't match everything.
func labelMatches(nameWords, labelWords []string) bool {
	if len(labelWords) == 0 {
		return false
	}
	if len(labelWords) == 1 && len(labelWords[0]) < 4 {
		return false
	}
	if len(labelWords) > len(nameWords) {
		return false
	}
	for i := 0; i+len(labelWords) <= len(nameWords); i++ {
		match := true
		for j := range labelWords {
			if nameWords[i+j] != labelWords[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// parseAliasList decodes performer_cache.aliases (a JSON string array);
// returns nil on empty/invalid.
func parseAliasList(j string) []string {
	if j == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(j), &out) != nil {
		return nil
	}
	return out
}
