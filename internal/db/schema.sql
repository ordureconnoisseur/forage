-- forager schema. Migrations run on every db.Open() via //go:embed; all
-- statements must be idempotent.
--
-- Phase 1 surface only: caches for the performer-list endpoint. Grab
-- history, match candidates, and owned-scene caches arrive with their
-- respective build steps.

-- performer_cache holds the local-library performer list plus a few
-- aggregates computed during the 12h scene-cache refresh:
--   total_stashdb_scenes  count of scenes on StashDB featuring this performer
--   owned_scenes_count    of those, how many the user already has
--   last_release_unix     newest StashDB scene's date (parsed to unix seconds)
-- These power the /performers `sort=last_release` and `sort=missing_count`
-- options. missing_count is derived in the query as
-- (total_stashdb_scenes - owned_scenes_count) — no stored column.
CREATE TABLE IF NOT EXISTS performer_cache (
  stash_id              TEXT PRIMARY KEY,
  stashdb_id            TEXT,
  name                  TEXT NOT NULL,
  aliases               TEXT,                    -- JSON array
  favorite              INTEGER NOT NULL DEFAULT 0,
  scene_count           INTEGER NOT NULL DEFAULT 0,
  total_stashdb_scenes  INTEGER NOT NULL DEFAULT 0,
  owned_scenes_count    INTEGER NOT NULL DEFAULT 0,
  last_release_unix     INTEGER NOT NULL DEFAULT 0,
  refreshed_at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_performer_favorite     ON performer_cache(favorite);
CREATE INDEX IF NOT EXISTS idx_performer_scene_count  ON performer_cache(scene_count);
CREATE INDEX IF NOT EXISTS idx_performer_last_release ON performer_cache(last_release_unix DESC);

CREATE TABLE IF NOT EXISTS studio_cache (
  stashdb_id   TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  aliases      TEXT,                    -- JSON array
  refreshed_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Phase B — grab tracking.
-- Each row is one user-initiated forager → download-client add. The
-- background poller advances the status: queued → downloading →
-- completed → confirmed | mismatched | orphaned (or failed).
--
-- `client` is the download-client kind ("qbit" | "sabnzbd"). `client_id`
-- holds qBit's info_hash for torrents and SAB's nzo_id for usenet.
-- `client_name` is the on-disk name the client reports — used by the
-- placer (and by Stash-side confirmation) to find the finished file.
--
-- `performer_name` is the folder forage will drop the finished file
-- into under <library_root>. Captured at /grab time from whichever
-- performer page the user grabbed from — predictable rather than
-- arr-stack "primary performer" heuristics.
--
-- `placed_path` records where the file landed after a successful
-- hardlink/copy into the library. `place_error` captures the failure
-- reason if placement didn't succeed, so the UI can surface it.
CREATE TABLE IF NOT EXISTS grabs (
  id                    INTEGER PRIMARY KEY,
  predicted_stashdb_id  TEXT,
  predicted_confidence  REAL,
  release_title         TEXT NOT NULL,
  release_size          INTEGER,
  release_indexer       TEXT,
  download_url          TEXT,
  client_id             TEXT,
  client_name           TEXT,
  client                TEXT NOT NULL DEFAULT 'qbit',
  category              TEXT,
  status                TEXT NOT NULL DEFAULT 'queued',
  actual_stashdb_id     TEXT,
  reason                TEXT,
  performer_name        TEXT,
  placed_path           TEXT,
  place_error           TEXT,
  grabbed_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL,
  completed_at          INTEGER,
  placed_at             INTEGER,
  confirmed_at          INTEGER,
  -- 'single' (one release → one scene) or 'pack' (one release → many
  -- of a performer's scenes; distinct confirm/dedup path in the poller).
  kind                  TEXT NOT NULL DEFAULT 'single',
  -- pack progress counters (0 for single grabs)
  pack_files            INTEGER NOT NULL DEFAULT 0,
  pack_identified       INTEGER NOT NULL DEFAULT 0,
  pack_deduped          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_grabs_status     ON grabs(status);
CREATE INDEX IF NOT EXISTS idx_grabs_client_id  ON grabs(client_id);
CREATE INDEX IF NOT EXISTS idx_grabs_grabbed_at ON grabs(grabbed_at DESC);

-- recent_scene_cache: one row per StashDB scene that features ≥1 of
-- the user's local performers AND is within the recent window
-- (hardcoded 90d). Rebuilt during the 12h refresh — pruned rows
-- outside the window each pass. local_performer_ids is a JSON array
-- of local stash_ids, the intersection of the scene's StashDB
-- performer list with performer_cache.stashdb_id. Chosen over a
-- normalized link table because our only access pattern is "render
-- this scene with its library performer chips" — we never JOIN by
-- performer.
CREATE TABLE IF NOT EXISTS recent_scene_cache (
  stashdb_id          TEXT PRIMARY KEY,
  title               TEXT,
  release_date        TEXT,                  -- YYYY-MM-DD as StashDB returns
  release_unix        INTEGER NOT NULL,      -- parsed; the sort key
  studio_name         TEXT,
  image_url           TEXT,
  local_performer_ids TEXT NOT NULL,         -- JSON array of local stash_id
  owned               INTEGER NOT NULL DEFAULT 0,
  cached_at           INTEGER NOT NULL,
  -- trending_rank: 0 when this scene isn't in the StashDB trending top-N,
  -- 1..N when it is (lower = more trending). Updated on a 1h cadence
  -- by cache.RefreshTrending, separate from the 12h performer-filtered
  -- refresh. Lets trending scenes coexist with performer-filtered ones
  -- in a single table — frequently the same scenes anyway.
  trending_rank       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_recent_release  ON recent_scene_cache(release_unix DESC);
CREATE INDEX IF NOT EXISTS idx_recent_owned    ON recent_scene_cache(owned);
CREATE INDEX IF NOT EXISTS idx_recent_trending ON recent_scene_cache(trending_rank);

