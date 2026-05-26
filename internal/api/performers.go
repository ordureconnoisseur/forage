package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/cache"
)

type performerOut struct {
	StashID    string   `json:"stash_id"`
	StashDBID  string   `json:"stashdb_id,omitempty"`
	Name       string   `json:"name"`
	Aliases    []string `json:"aliases"`
	Favorite   bool     `json:"favorite"`
	SceneCount int      `json:"scene_count"`
	// Aggregates set by cache.RefreshSceneCache on the 12h ticker.
	// Zero when the scene cache has never run, or when the performer
	// has no StashDB cross-id (so we couldn't query their filmography).
	TotalStashDBScenes int   `json:"total_stashdb_scenes"`
	OwnedScenesCount   int   `json:"owned_scenes_count"`
	LastReleaseUnix    int64 `json:"last_release_unix"`
}

type performersResponse struct {
	Performers  []performerOut `json:"performers"`
	RefreshedAt int64          `json:"refreshed_at"`
}

func (s *Server) getPerformers(w http.ResponseWriter, r *http.Request) {
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

	rows, args := buildPerformerQuery(orderBy, favoriteOnly, q)
	out, err := readPerformers(r.Context(), s.db, rows, args)
	if err != nil {
		s.log.Error("getPerformers query", "err", err)
		writeErr(w, http.StatusInternalServerError, "db")
		return
	}

	refreshedAt, _ := cache.PerformerRefreshedAt(r.Context(), s.db)
	writeJSON(w, http.StatusOK, performersResponse{
		Performers:  out,
		RefreshedAt: refreshedAt,
	})
}

func sortClause(sort string) (string, bool) {
	switch sort {
	case "scene_count":
		return "scene_count DESC, name COLLATE NOCASE ASC", true
	case "name":
		return "name COLLATE NOCASE ASC", true
	case "last_release":
		// Performers without a StashDB cross-id have last_release_unix
		// = 0 and sink to the bottom; name tiebreak so the order is
		// stable when several share the same recent date.
		return "last_release_unix DESC, name COLLATE NOCASE ASC", true
	case "missing_count":
		// SQLite evaluates the expression at scan time. Performers
		// without a StashDB cross-id have 0-0=0 and sink to bottom.
		return "(total_stashdb_scenes - owned_scenes_count) DESC, name COLLATE NOCASE ASC", true
	}
	return "", false
}

func buildPerformerQuery(orderBy string, favoriteOnly bool, q string) (string, []any) {
	var where []string
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
	return "SELECT stash_id, stashdb_id, name, aliases, favorite, scene_count, " +
		"total_stashdb_scenes, owned_scenes_count, last_release_unix " +
		"FROM performer_cache " + whereSQL + " ORDER BY " + orderBy, args
}

func readPerformers(ctx context.Context, db *sql.DB, query string, args []any) ([]performerOut, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []performerOut{}
	for rows.Next() {
		var p performerOut
		var stashdbID sql.NullString
		var aliasesJSON sql.NullString
		var fav int
		if err := rows.Scan(&p.StashID, &stashdbID, &p.Name, &aliasesJSON, &fav, &p.SceneCount,
			&p.TotalStashDBScenes, &p.OwnedScenesCount, &p.LastReleaseUnix); err != nil {
			return nil, err
		}
		if stashdbID.Valid {
			p.StashDBID = stashdbID.String
		}
		p.Favorite = fav != 0
		p.Aliases = []string{}
		if aliasesJSON.Valid && aliasesJSON.String != "" && aliasesJSON.String != "null" {
			_ = json.Unmarshal([]byte(aliasesJSON.String), &p.Aliases)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
