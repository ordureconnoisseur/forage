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

// UpsertSceneBatch writes a fetched batch of StashDB scenes into the persistent
// cache in one transaction. Safe to call concurrently from the refresh workers —
// the single DB connection serializes the transactions (fetches stay parallel;
// only the writes queue).
func UpsertSceneBatch(ctx context.Context, db *sql.DB, scenes []stashdb.Scene, now int64) error {
	if len(scenes) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := range scenes {
		if err := UpsertScene(ctx, tx, scenes[i], now); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// RebuildRecentSceneCache regenerates recent_scene_cache (the Discover source)
// from the persistent scene cache: every cached scene inside the recency window
// that features at least one owned performer, with that scene's owned-performer
// local ids and an owned flag. Trending rows (trending_rank > 0, managed by
// RefreshTrending) are preserved. Needed once the delta sync stops fetching the
// full per-pass scene set — Discover now reads from the accumulated cache.
func RebuildRecentSceneCache(ctx context.Context, db *sql.DB, cutoff, start int64, ownedIDs []string) error {
	ownedSet := make(map[string]bool, len(ownedIDs))
	for _, id := range ownedIDs {
		ownedSet[id] = true
	}

	// One JOIN yields (scene, owned-performer-local-id) rows; group in Go to
	// collect each recent scene's owned-performer local ids.
	rows, err := db.QueryContext(ctx, `
		SELECT s.stashdb_id, COALESCE(s.title,''), COALESCE(s.release_date,''),
		       s.release_unix, COALESCE(s.studio_name,''), COALESCE(s.image_url,''),
		       pc.stash_id
		FROM stashdb_scene s
		JOIN scene_performer sp ON sp.scene_id = s.stashdb_id
		JOIN performer_cache pc ON pc.stashdb_id = sp.performer_stashdb_id
		WHERE s.release_unix >= ?`, cutoff)
	if err != nil {
		return err
	}
	type rec struct {
		title, date, studio, image string
		releaseUnix                int64
		localIDs                   []string
	}
	recs := map[string]*rec{}
	order := []string{}
	for rows.Next() {
		var id, title, date, studio, image, localID string
		var ru int64
		if err := rows.Scan(&id, &title, &date, &ru, &studio, &image, &localID); err != nil {
			rows.Close()
			return err
		}
		r := recs[id]
		if r == nil {
			r = &rec{title: title, date: date, studio: studio, image: image, releaseUnix: ru}
			recs[id] = r
			order = append(order, id)
		}
		if localID != "" {
			r.localIDs = append(r.localIDs, localID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO recent_scene_cache (
		  stashdb_id, title, release_date, release_unix,
		  studio_name, image_url, local_performer_ids, owned, cached_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(stashdb_id) DO UPDATE SET
		  title=excluded.title, release_date=excluded.release_date,
		  release_unix=excluded.release_unix, studio_name=excluded.studio_name,
		  image_url=excluded.image_url, local_performer_ids=excluded.local_performer_ids,
		  owned=excluded.owned, cached_at=excluded.cached_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range order {
		r := recs[id]
		idsJSON, _ := json.Marshal(r.localIDs)
		ownedFlag := 0
		if ownedSet[id] {
			ownedFlag = 1
		}
		if _, err := stmt.ExecContext(ctx, id, r.title, r.date, r.releaseUnix,
			r.studio, r.image, string(idsJSON), ownedFlag, start); err != nil {
			return err
		}
	}

	// Prune: rows outside the window or not re-stamped this pass, except
	// trending rows (those are RefreshTrending's to manage).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM recent_scene_cache WHERE (release_unix < ? OR cached_at < ?) AND trending_rank <= 0`,
		cutoff, start); err != nil {
		return err
	}
	return tx.Commit()
}
