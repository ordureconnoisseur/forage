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
  -- Stash performer gender enum ('' when unset); drives Discover's
  -- configurable content filters.
  gender                TEXT NOT NULL DEFAULT '',
  total_stashdb_scenes  INTEGER NOT NULL DEFAULT 0,
  owned_scenes_count    INTEGER NOT NULL DEFAULT 0,
  last_release_unix     INTEGER NOT NULL DEFAULT 0,
  -- max StashDB `updated` seen for this performer's scenes; the delta sync
  -- stops paginating once it reaches scenes older than this (0 = full sync).
  scenes_synced_at      INTEGER NOT NULL DEFAULT 0,
  refreshed_at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_performer_favorite     ON performer_cache(favorite);
CREATE INDEX IF NOT EXISTS idx_performer_scene_count  ON performer_cache(scene_count);
CREATE INDEX IF NOT EXISTS idx_performer_last_release ON performer_cache(last_release_unix DESC);

-- studio_cache holds the local-library studio list. Keyed by the StashDB
-- cross-id (or a synthetic "stash:<local_id>" for studios with no cross-id).
-- name + aliases feed the matcher (matcher.LoadStudios reads only those). The
-- remaining columns mirror performer_cache and power the /studios page:
--   scene_count           local owned scenes catalogued under this studio
--   total_stashdb_scenes  count of scenes on StashDB for this studio
--   owned_scenes_count    of those, how many the user already has
--   last_release_unix     newest StashDB scene's date (unix seconds)
-- The aggregates are filled by cache.RefreshStudioCache on the 12h ticker
-- (only for studios with a real StashDB cross-id). missing_count is derived
-- as (total_stashdb_scenes - owned_scenes_count) in the query.
CREATE TABLE IF NOT EXISTS studio_cache (
  stashdb_id            TEXT PRIMARY KEY,
  stash_id              TEXT,                    -- local Stash studio id
  name                  TEXT NOT NULL,
  aliases               TEXT,                    -- JSON array
  favorite              INTEGER NOT NULL DEFAULT 0,
  scene_count           INTEGER NOT NULL DEFAULT 0,
  -- Stash performer gender enum ('' when unset); drives Discover's
  -- configurable content filters.
  gender                TEXT NOT NULL DEFAULT '',
  total_stashdb_scenes  INTEGER NOT NULL DEFAULT 0,
  owned_scenes_count    INTEGER NOT NULL DEFAULT 0,
  last_release_unix     INTEGER NOT NULL DEFAULT 0,
  -- delta-sync watermark (see performer_cache.scenes_synced_at).
  scenes_synced_at      INTEGER NOT NULL DEFAULT 0,
  refreshed_at          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_studio_favorite     ON studio_cache(favorite);
CREATE INDEX IF NOT EXISTS idx_studio_scene_count  ON studio_cache(scene_count);
CREATE INDEX IF NOT EXISTS idx_studio_last_release ON studio_cache(last_release_unix DESC);

-- stashdb_scene is the PERSISTENT cache of StashDB scene bodies (immutable
-- after publication), keyed by the StashDB scene id. The performer/studio
-- pages read their filmographies from here instead of live-querying StashDB,
-- and the delta sync upserts into it (a scene seen via both a performer and a
-- studio query is stored once). performers/urls/tags are denormalised JSON for
-- rendering without a join; scene_performer (below) carries the membership the
-- performer page filters on. updated_unix is StashDB's `updated` timestamp —
-- the delta sync's stop signal.
CREATE TABLE IF NOT EXISTS stashdb_scene (
  stashdb_id    TEXT PRIMARY KEY,
  title         TEXT,
  release_date  TEXT,
  release_unix  INTEGER NOT NULL DEFAULT 0,
  studio_id     TEXT,                       -- StashDB studio id (studio-page membership)
  studio_name   TEXT,
  image_url     TEXT,
  performers    TEXT NOT NULL DEFAULT '[]', -- JSON [{id,name,as}]
  urls          TEXT NOT NULL DEFAULT '[]', -- JSON [{url,type}]
  tags          TEXT NOT NULL DEFAULT '[]', -- JSON [name,...]
  updated_unix  INTEGER NOT NULL DEFAULT 0, -- StashDB `updated`
  cached_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_scene_studio  ON stashdb_scene(studio_id);
CREATE INDEX IF NOT EXISTS idx_scene_release ON stashdb_scene(release_unix DESC);

-- scene_performer is the many-to-many membership (a scene has several
-- performers): the performer page filters by performer_stashdb_id.
CREATE TABLE IF NOT EXISTS scene_performer (
  scene_id              TEXT NOT NULL,
  performer_stashdb_id  TEXT NOT NULL,
  PRIMARY KEY (scene_id, performer_stashdb_id)
);
CREATE INDEX IF NOT EXISTS idx_sp_performer ON scene_performer(performer_stashdb_id);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Phase B — grab tracking.
-- Each row is one user-initiated forager → download-client add. The
-- background poller advances the status: queued → downloading →
-- completed → confirmed | mismatched | orphaned (or failed).
--
-- `deferred` is the retry-pending state between queued and failed: an
-- add that failed TRANSIENTLY (download client unreachable, indexer
-- 5xx) parks here instead of failing outright. `attempts` counts failed
-- add attempts so far; `next_retry_at` is when the deferred-retry loop
-- may re-drive the add (exponential backoff). Once the attempt budget
-- is spent the grab settles to failed with the final error in `reason`.
-- Deferred grabs are deliberately NOT in the poller's Active() set: the
-- add never landed, so the link-timeout sweep would otherwise fail them
-- while they wait for their retry slot.
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
  pack_deduped          INTEGER NOT NULL DEFAULT 0,
  -- download-progress tracking for stalled detection (qBit torrents):
  -- progress is 0..1; progress_at is the unix time it last increased.
  progress              REAL NOT NULL DEFAULT 0,
  progress_at           INTEGER NOT NULL DEFAULT 0,
  -- deferred-retry state (status='deferred'): failed add attempts so
  -- far, and the unix time the retry loop may try again. 0/NULL for
  -- grabs that never deferred.
  attempts              INTEGER NOT NULL DEFAULT 0,
  next_retry_at         INTEGER,
  -- which stage the deferred failure happened in: 'indexer' (the
  -- .torrent fetch through Prowlarr failed; worth failing over to the
  -- same scene's release on a different indexer) or 'client' (the
  -- download client couldn't take it; retry the same release). Empty
  -- for grabs that never deferred.
  fail_kind             TEXT,
  -- optimistic-lock version: bumped on every Update; the WHERE clause
  -- matches on it so a stale writer (poller tick vs concurrent API edit)
  -- loses instead of clobbering. (Physical column order no longer
  -- matters to reads: the repo rewrites SELECT * to an explicit column
  -- list, so migrated DBs whose ALTER-appended columns land after rev
  -- scan identically to fresh ones.)
  rev                   INTEGER NOT NULL DEFAULT 0
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

-- watches: user-tracked StashDB scenes. A watch says "tell me when this
-- scene has a release at quality `target`". A background loop re-searches
-- watching rows on a spread-over-24h cadence; on a verified release that
-- matches the target resolution it flips status watching → available,
-- records the found release AND the full verified candidate list (so the
-- user can re-pick a different release than the auto-chosen best). It never
-- grabs — the user grabs from the Watching tab; grabbing flips the watch to
-- `grabbed` (it lingers, it is NOT deleted, so a batch's progress reads
-- correctly). Denormalised scene display fields so the tab renders without
-- a StashDB round-trip.
--
-- Batches: watches created together in one request (e.g. "watch all missing
-- for a performer", or a Discover multi-select) share a batch_id, so the
-- Watching tab can group them with a progress line. A batch is emergent from
-- GROUP BY batch_id — there is no separate batch entity.
CREATE TABLE IF NOT EXISTS watches (
  stashdb_id     TEXT PRIMARY KEY,
  title          TEXT,
  date           TEXT,
  studio_name    TEXT,
  image_url      TEXT,
  performer_name TEXT,              -- the performer the user tracked from (folder placement)
  performer_id   TEXT,              -- local stash_id of that performer
  -- ALL of the scene's performer names (JSON array), captured at add time so a
  -- re-search never re-fetches the scene from StashDB (avoids throttling) AND
  -- searches every performer — releases are often named under a non-tracked
  -- performer. Empty until resolved (then the loop backfills it once).
  performers     TEXT NOT NULL DEFAULT '[]',
  -- target resolution is vestigial (the matcher no longer filters on it;
  -- releases are ranked by preference score). Kept for schema compatibility.
  target         TEXT NOT NULL DEFAULT 'any',
  status         TEXT NOT NULL DEFAULT 'watching', -- watching | available | grabbed
  -- When available, the release that satisfied it (for one-click grab).
  found_title    TEXT,
  found_url      TEXT,
  found_indexer  TEXT,
  found_protocol TEXT,
  found_size     INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL,
  last_checked   INTEGER NOT NULL DEFAULT 0, -- unix; 0 = never. The loop claims oldest first.
  found_at       INTEGER NOT NULL DEFAULT 0,
  -- download URLs the user dismissed for this watch (JSON array). The watch
  -- loop skips these so a rejected dead/over-compressed find can't
  -- re-surface; the watch keeps looking for a different release.
  ignored_urls   TEXT NOT NULL DEFAULT '[]',
  -- batch grouping (see header). batch_id empty = an ungrouped single track.
  batch_id       TEXT NOT NULL DEFAULT '',
  batch_label    TEXT NOT NULL DEFAULT '',
  -- the full verified candidate list captured when the watch flipped
  -- available (JSON array of releases), so the user can re-pick. Empty until
  -- available.
  candidates     TEXT NOT NULL DEFAULT '[]',
  -- when the watch was grabbed (status='grabbed'); 0 otherwise.
  grabbed_at     INTEGER NOT NULL DEFAULT 0,
  -- how many times this scene has been re-searched for a release (incremented
  -- on every loop claim and manual search-now). Surfaced in the Watching tab
  -- so you can see how hard forage has tried on a still-unfound scene.
  search_count   INTEGER NOT NULL DEFAULT 0,
  -- upgrade watches: 0 = normal acquire watch; >0 = the owned copy's
  -- height when the watch was created, and only releases EXCEEDING it
  -- may flip the watch available. An upgrade watch is DONE when an
  -- owned copy exceeds the floor (not when "a grab exists": the scene
  -- is owned by definition, usually via an old confirmed grab).
  upgrade_floor  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_watches_status  ON watches(status);
CREATE INDEX IF NOT EXISTS idx_watches_checked ON watches(last_checked ASC);
CREATE INDEX IF NOT EXISTS idx_watches_batch   ON watches(batch_id);

-- rss_sync_state: the RSS-sync high-watermark per indexer — the most-recent
-- release publishDate (unix) the sync has already processed for that indexer.
-- The RSS loop pulls each indexer's recent-uploads feed and only matches
-- releases newer than this mark (minus a small overlap), so each tick handles
-- just the genuinely-new uploads instead of re-matching the whole recent
-- window. Keyed by Prowlarr's indexer id; absent row = never synced (first run
-- uses a bounded lookback instead of the watermark).
CREATE TABLE IF NOT EXISTS rss_sync_state (
  indexer_id        INTEGER PRIMARY KEY,
  last_publish_unix INTEGER NOT NULL DEFAULT 0
);

-- pack_duplicate: one row per (pack grab, StashDB scene) collision that the
-- review-mode dedup path (PackDedupKeep="review") found — a scene the pack
-- delivered that the library ALREADY had elsewhere. Instead of the auto
-- keep="pack" path destroying a pre-existing copy unattended, review mode
-- records the collision here as `pending` and waits for the user to choose
-- per scene which copy survives. The resolve endpoint then runs the
-- (irreversible) SceneDestroy in the foreground on the chosen side. This is
-- the only dedup path that can delete a pre-existing library copy, and it
-- never does so without an explicit decision.
--
-- existing_copies is a JSON array of the pre-existing copies
-- ({scene_id,title,path,size,height}); pack_* hold the pack's own copy. The
-- UI compares pack vs existing (resolution/size/path) so the user keeps the
-- better file. UNIQUE(grab_id, stashdb_id) makes detection idempotent — a
-- re-tick upserts rather than duplicating.
-- Destruction journal: one row per scene destruction forage attempts,
-- WRITTEN BEFORE the destroy runs (outcome 'intent', finalised to
-- 'destroyed'/'failed' after), plus one row per refusal. The answer to
-- "what did forage delete, when, and why" without log archaeology — and if
-- the process dies mid-destroy, the intent row is the evidence of what was
-- in flight. files is a JSON array [{path,size}] snapshotting the complete
-- blast radius at decision time.
CREATE TABLE IF NOT EXISTS destruction_log (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  reason     TEXT NOT NULL,                 -- surface: 'grab purge', 'pack dedup', …
  scene_id   TEXT NOT NULL DEFAULT '',
  title      TEXT NOT NULL DEFAULT '',
  files      TEXT NOT NULL DEFAULT '[]',    -- JSON [{path,size}]
  outcome    TEXT NOT NULL DEFAULT 'intent',-- intent | destroyed | failed | refused
  detail     TEXT NOT NULL DEFAULT '',      -- error text / refusal reason
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_destruction_log_created ON destruction_log(created_at);

CREATE TABLE IF NOT EXISTS pack_duplicate (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  grab_id         INTEGER NOT NULL,
  stashdb_id      TEXT NOT NULL,
  scene_title     TEXT NOT NULL DEFAULT '',
  pack_scene_id   TEXT NOT NULL,
  pack_path       TEXT NOT NULL DEFAULT '',
  pack_size       INTEGER NOT NULL DEFAULT 0,
  pack_height     INTEGER NOT NULL DEFAULT 0,
  existing_copies TEXT NOT NULL DEFAULT '[]', -- JSON [{scene_id,title,path,size,height}]
  status          TEXT NOT NULL DEFAULT 'pending', -- pending | resolved
  resolution      TEXT NOT NULL DEFAULT '',         -- existing | pack | both (once resolved)
  created_at      INTEGER NOT NULL,
  resolved_at     INTEGER NOT NULL DEFAULT 0,
  UNIQUE(grab_id, stashdb_id)
);
CREATE INDEX IF NOT EXISTS idx_pack_dup_status ON pack_duplicate(status);
CREATE INDEX IF NOT EXISTS idx_pack_dup_grab   ON pack_duplicate(grab_id);

-- Dismiss review items with no existing side. Nothing can be done with one:
-- the UI renders "?" where the other copy's resolution/size/path belong,
-- "keep existing" can only 409 (the side it would keep isn't in Stash), and
-- "keep new" destroys nothing yet marks itself resolved.
--
-- They came from counting FILES as copies: FindSceneRefsByStashID emits one
-- ref per file, and Stash files a matching-fingerprint re-download as a
-- second file on the SAME scene, so a lone scene looked like two copies and
-- every ref landed on the pack side. Both the detector and the writer now
-- refuse the case, so this only ever clears the backlog — but it runs on
-- every open (schema.sql is re-executed), which keeps it self-healing and
-- costs nothing once the set is empty.
UPDATE pack_duplicate
   SET status = 'resolved', resolution = 'both', resolved_at = strftime('%s', 'now')
 WHERE status = 'pending'
   AND TRIM(COALESCE(existing_copies, '')) IN ('', '[]', 'null');


-- Subscriptions: permanent performer/studio watches ("subscribe to
-- Kenzie Reeves"). Where a scene watch tracks ONE scene and a
-- collection job sweeps the BACKLOG once, a subscription covers the
-- future: the subscription loop diffs the subject's StashDB scene list
-- against `watermark` (newest release date already processed) and
-- creates ordinary scene watches for anything new, batch-tagged
-- 'sub:<stashdb_id>' so the whole existing watch machinery (re-search,
-- RSS, available, notify, grab) takes over. Backlog is deliberately NOT
-- included (watermark initialises to the subscribe date); the UI offers
-- the existing watch-all-missing for that.
--
-- `auto_grab` opts one subscription into hands-free grabbing: the loop
-- grabs its batch's available watches through the same path as the UI
-- button (benched-indexer filter included). `last_seen_at` drives the
-- Grabs-tab rail badge: batch activity after this stamp counts as new.
CREATE TABLE IF NOT EXISTS subscriptions (
  stashdb_id   TEXT PRIMARY KEY,
  kind         TEXT NOT NULL DEFAULT 'performer', -- 'performer' | 'studio'
  name         TEXT NOT NULL,
  -- local_id: the subject's LOCAL Stash id. The loop works purely by
  -- cross-id; the plugin navigates subject pages and proxies portrait
  -- images by local id.
  local_id     TEXT NOT NULL DEFAULT '',
  image_url    TEXT,
  auto_grab    INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  last_checked INTEGER NOT NULL DEFAULT 0,
  watermark    INTEGER NOT NULL DEFAULT 0,
  -- seen_total mirrors the subject's total_stashdb_scenes aggregate at
  -- the last processed pass. Change detection needs BOTH signals: a new
  -- scene with a NEWER date moves last_release_unix past the watermark,
  -- but a scene added later carrying the SAME newest date only moves the
  -- count. Without it, steady state (watermark == last release) would
  -- either re-fetch every subject's scene list hourly or miss same-day
  -- additions entirely.
  seen_total   INTEGER NOT NULL DEFAULT 0,
  last_seen_at INTEGER NOT NULL DEFAULT 0
);

-- Built-in torrent engine (internal/engine): one row per torrent the
-- engine has ever been handed. uploaded and seed_seconds are CUMULATIVE
-- across daemon restarts (the live client's session stats reset every
-- boot; the accounting loop folds deltas in here) — they are what the
-- seeding cull's ratio/age verdicts read.
CREATE TABLE IF NOT EXISTS engine_torrents (
  info_hash    TEXT PRIMARY KEY,               -- lowercase hex
  name         TEXT NOT NULL DEFAULT '',
  category     TEXT NOT NULL DEFAULT '',
  added_on     INTEGER NOT NULL,
  completed_on INTEGER NOT NULL DEFAULT 0,     -- unix; 0 while incomplete
  total_size   INTEGER NOT NULL DEFAULT 0,
  uploaded     INTEGER NOT NULL DEFAULT 0,     -- cumulative bytes
  seed_seconds INTEGER NOT NULL DEFAULT 0      -- cumulative active seeding
);
