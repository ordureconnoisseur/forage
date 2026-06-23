package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/cache"
)

// studioOut is one row of the /studios list — the studio analogue of
// performerOut. The navigation id is StashDBID (the studio_cache key); for
// studios with no StashDB cross-id it's a synthetic "stash:<local_id>" and
// the aggregates stay zero (we can't query StashDB for their catalogue).
type studioOut struct {
	StashDBID  string   `json:"stashdb_id"`
	StashID    string   `json:"stash_id,omitempty"`
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases"`
	Favorite   bool     `json:"favorite"`
	SceneCount int      `json:"scene_count"`
	// Aggregates set by cache.RefreshStudioCache on the 12h ticker. Zero
	// until it has run, or for studios with no StashDB cross-id.
	TotalStashDBScenes int   `json:"total_stashdb_scenes"`
	OwnedScenesCount   int   `json:"owned_scenes_count"`
	LastReleaseUnix    int64 `json:"last_release_unix"`
}

type studiosResponse struct {
	Studios     []studioOut `json:"studios"`
	RefreshedAt int64       `json:"refreshed_at"`
}

func (s *Server) getStudios(w http.ResponseWriter, r *http.Request) {
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "scene_count"
	}
	orderBy, ok := sortClause(sort)
	if !ok {
		writeErr(w, http.StatusBadRequest, "sort must be one of: scene_count, name, last_release, missing_count")
		return
	}
	favoriteOnly := r.URL.Query().Get("favorite_only") == "true"
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	query, args := buildStudioQuery(orderBy, favoriteOnly, q)
	out, err := readStudios(r.Context(), s.db, query, args)
	if err != nil {
		s.log.Error("getStudios query", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}

	refreshedAt, _ := cache.StudioRefreshedAt(r.Context(), s.db)
	writeJSON(w, http.StatusOK, studiosResponse{Studios: out, RefreshedAt: refreshedAt})
}

func buildStudioQuery(orderBy string, favoriteOnly bool, q string) (string, []any) {
	// Only OWNED studios — ones the user actually has scenes from. studio_cache
	// also holds studios that exist in Stash purely as scraped metadata
	// (scene_count 0); those aren't "your studios" and would bloat the list.
	where := []string{"scene_count > 0"}
	var args []any
	if favoriteOnly {
		where = append(where, "favorite = 1")
	}
	if q != "" {
		where = append(where, "(LOWER(name) LIKE ? OR LOWER(aliases) LIKE ?)")
		needle := "%" + strings.ToLower(q) + "%"
		args = append(args, needle, needle)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	return "SELECT stashdb_id, stash_id, name, aliases, favorite, scene_count, " +
		"total_stashdb_scenes, owned_scenes_count, last_release_unix " +
		"FROM studio_cache " + whereSQL + " ORDER BY " + orderBy, args
}

func readStudios(ctx context.Context, db *sql.DB, query string, args []any) ([]studioOut, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []studioOut{}
	for rows.Next() {
		var st studioOut
		var stashID, aliasesJSON sql.NullString
		var fav int
		if err := rows.Scan(&st.StashDBID, &stashID, &st.Name, &aliasesJSON, &fav, &st.SceneCount,
			&st.TotalStashDBScenes, &st.OwnedScenesCount, &st.LastReleaseUnix); err != nil {
			return nil, err
		}
		if stashID.Valid {
			st.StashID = stashID.String
		}
		st.Favorite = fav != 0
		st.Aliases = []string{}
		if aliasesJSON.Valid && aliasesJSON.String != "" && aliasesJSON.String != "null" {
			_ = json.Unmarshal([]byte(aliasesJSON.String), &st.Aliases)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
