package cache

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// The persistent StashDB scene cache. StashDB scenes are immutable after
// publication, so we store their bodies once (stashdb_scene) plus the
// performer membership (scene_performer) and serve the performer/studio pages
// from here instead of re-querying StashDB every visit. The delta sync upserts
// into it; a scene seen via both a performer query and a studio query is stored
// once (upsert by id).

// scenePerf / sceneURLRow are the denormalised JSON shapes stored on the row.
type scenePerf struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	As   string `json:"as,omitempty"`
}
type sceneURLRow struct {
	URL  string `json:"url"`
	Type string `json:"type,omitempty"`
}

// sceneExecer is satisfied by both *sql.DB and *sql.Tx so UpsertScene can run
// inside the sync's transaction or standalone.
type sceneExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpsertScene writes a StashDB scene body into stashdb_scene and re-syncs its
// scene_performer membership. Idempotent (upsert by id).
func UpsertScene(ctx context.Context, ex sceneExecer, s stashdb.Scene, now int64) error {
	perfs := make([]scenePerf, 0, len(s.Performers))
	for _, p := range s.Performers {
		perfs = append(perfs, scenePerf{ID: p.ID, Name: p.Name, As: p.As})
	}
	perfsJSON, _ := json.Marshal(perfs)
	urls := make([]sceneURLRow, 0, len(s.URLs))
	for _, u := range s.URLs {
		urls = append(urls, sceneURLRow{URL: u.URL, Type: u.Type})
	}
	urlsJSON, _ := json.Marshal(urls)
	tags := s.Tags
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, _ := json.Marshal(tags)
	studioID, studioName := "", ""
	if s.Studio != nil {
		studioID, studioName = s.Studio.ID, s.Studio.Name
	}
	img := ""
	if len(s.Images) > 0 {
		img = s.Images[0].URL
	}
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO stashdb_scene (
		  stashdb_id, title, release_date, release_unix, studio_id, studio_name,
		  image_url, performers, urls, tags, updated_unix, cached_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stashdb_id) DO UPDATE SET
		  title=excluded.title, release_date=excluded.release_date,
		  release_unix=excluded.release_unix, studio_id=excluded.studio_id,
		  studio_name=excluded.studio_name, image_url=excluded.image_url,
		  performers=excluded.performers, urls=excluded.urls, tags=excluded.tags,
		  updated_unix=excluded.updated_unix, cached_at=excluded.cached_at`,
		s.ID, s.Title, s.Date, parseStashDBDate(s.Date), studioID, studioName,
		img, string(perfsJSON), string(urlsJSON), string(tagsJSON), s.Updated, now,
	); err != nil {
		return err
	}
	// Re-sync membership (a scene's performer set can change between syncs).
	if _, err := ex.ExecContext(ctx, `DELETE FROM scene_performer WHERE scene_id = ?`, s.ID); err != nil {
		return err
	}
	for _, p := range s.Performers {
		if p.ID == "" {
			continue
		}
		if _, err := ex.ExecContext(ctx,
			`INSERT OR IGNORE INTO scene_performer (scene_id, performer_stashdb_id) VALUES (?, ?)`,
			s.ID, p.ID); err != nil {
			return err
		}
	}
	return nil
}

const sceneCols = `s.stashdb_id, COALESCE(s.title,''), COALESCE(s.release_date,''),
	COALESCE(s.studio_id,''), COALESCE(s.studio_name,''), COALESCE(s.image_url,''),
	COALESCE(s.performers,'[]'), COALESCE(s.urls,'[]'), COALESCE(s.tags,'[]'), s.updated_unix`

// ScenesForPerformer reads a performer's cached filmography (by their StashDB
// id) from the scene cache — no live StashDB query.
func ScenesForPerformer(ctx context.Context, db *sql.DB, performerStashDBID string) ([]stashdb.Scene, error) {
	return queryCachedScenes(ctx, db, `
		SELECT `+sceneCols+` FROM stashdb_scene s
		JOIN scene_performer sp ON sp.scene_id = s.stashdb_id
		WHERE sp.performer_stashdb_id = ?
		ORDER BY s.release_unix DESC`, performerStashDBID)
}

// ScenesForStudio reads a studio's cached filmography (by StashDB studio id).
func ScenesForStudio(ctx context.Context, db *sql.DB, studioStashDBID string) ([]stashdb.Scene, error) {
	return queryCachedScenes(ctx, db, `
		SELECT `+sceneCols+` FROM stashdb_scene s
		WHERE s.studio_id = ?
		ORDER BY s.release_unix DESC`, studioStashDBID)
}

func queryCachedScenes(ctx context.Context, db *sql.DB, q string, args ...any) ([]stashdb.Scene, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stashdb.Scene
	for rows.Next() {
		var id, title, date, studioID, studioName, img, perfsJSON, urlsJSON, tagsJSON string
		var updated int64
		if err := rows.Scan(&id, &title, &date, &studioID, &studioName, &img,
			&perfsJSON, &urlsJSON, &tagsJSON, &updated); err != nil {
			return nil, err
		}
		s := stashdb.Scene{ID: id, Title: title, Date: date, Updated: updated}
		if studioID != "" || studioName != "" {
			s.Studio = &stashdb.SceneStudio{ID: studioID, Name: studioName}
		}
		if img != "" {
			s.Images = []stashdb.SceneImage{{URL: img}}
		}
		var perfs []scenePerf
		_ = json.Unmarshal([]byte(perfsJSON), &perfs)
		for _, p := range perfs {
			s.Performers = append(s.Performers, stashdb.ScenePerformer{ID: p.ID, Name: p.Name, As: p.As})
		}
		var urls []sceneURLRow
		_ = json.Unmarshal([]byte(urlsJSON), &urls)
		for _, u := range urls {
			s.URLs = append(s.URLs, stashdb.SceneURL{URL: u.URL, Type: u.Type})
		}
		_ = json.Unmarshal([]byte(tagsJSON), &s.Tags)
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecomputeAggregates recomputes the performer_cache and studio_cache scene
// aggregates (total / owned / last_release) purely from the persistent scene
// cache and the given owned StashDB scene-id set — no StashDB queries.
func RecomputeAggregates(ctx context.Context, db *sql.DB, ownedIDs []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`CREATE TEMP TABLE IF NOT EXISTS _owned_scene (stashdb_id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM _owned_scene`); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO _owned_scene (stashdb_id) VALUES (?)`)
	if err != nil {
		return err
	}
	for _, id := range ownedIDs {
		if _, err := ins.ExecContext(ctx, id); err != nil {
			ins.Close()
			return err
		}
	}
	ins.Close()

	// Performers: derived from scene_performer membership.
	if _, err := tx.ExecContext(ctx, `
		UPDATE performer_cache SET
		  total_stashdb_scenes = (SELECT COUNT(*) FROM scene_performer sp WHERE sp.performer_stashdb_id = performer_cache.stashdb_id),
		  owned_scenes_count   = (SELECT COUNT(*) FROM scene_performer sp JOIN _owned_scene o ON o.stashdb_id = sp.scene_id WHERE sp.performer_stashdb_id = performer_cache.stashdb_id),
		  last_release_unix    = COALESCE((SELECT MAX(s.release_unix) FROM scene_performer sp JOIN stashdb_scene s ON s.stashdb_id = sp.scene_id WHERE sp.performer_stashdb_id = performer_cache.stashdb_id), 0)
		WHERE stashdb_id IS NOT NULL AND stashdb_id != ''`); err != nil {
		return err
	}
	// Studios: derived from stashdb_scene.studio_id (one studio per scene).
	if _, err := tx.ExecContext(ctx, `
		UPDATE studio_cache SET
		  total_stashdb_scenes = (SELECT COUNT(*) FROM stashdb_scene s WHERE s.studio_id = studio_cache.stashdb_id),
		  owned_scenes_count   = (SELECT COUNT(*) FROM stashdb_scene s JOIN _owned_scene o ON o.stashdb_id = s.stashdb_id WHERE s.studio_id = studio_cache.stashdb_id),
		  last_release_unix    = COALESCE((SELECT MAX(s.release_unix) FROM stashdb_scene s WHERE s.studio_id = studio_cache.stashdb_id), 0)
		WHERE stashdb_id NOT LIKE 'stash:%'`); err != nil {
		return err
	}
	return tx.Commit()
}
