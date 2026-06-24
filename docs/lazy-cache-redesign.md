# Lazy scene cache + cheap counts (redesign)

Status: planned, not started. Supersedes the eager scene-cache sync that phases
1–5 shipped (commits `a19b1f5`…`eac68aa`). Tonight's eager cache works and stays
live until this lands.

## Verification results (2026-06-24) — read before building

Two owned-count approaches were shadow-tested against the live eager numbers
(via `GET /debug/owned-count-check` and `GET /debug/idcount-check`, both still
in the tree):

- **Local-tag attribution (DEAD).** Counting owned scenes from the *local*
  scene→studio/performer cross-ids undercounts badly — 402/757 performers
  differ, because local tagging is less complete than StashDB's matching (e.g.
  Jia Lissa: StashDB knows she's in 36 owned scenes, locally only 5 are tagged).
  Studios diverge both ways (parent vs sub-label). **Do not use local tags for
  owned counts.**
- **ID-only attribution (PROVEN — use this).** Fetch just the subject's scene
  *ids* from StashDB (`QuerySceneIDs`, ~10x lighter than bodies) and intersect
  with the owned set — same semantics as the eager pass. Sampled top-owned
  subjects matched **20/20 performers, 18/20 studios**; the 2 studio diffs are
  >5,000-scene mega-studios where both methods are cap-limited. The `count`
  field also returns the TRUE total (fixes the eager method's capped/inflated
  mega-studio totals). Because id payloads are light, **raise the id cap** (e.g.
  20k) so even mega-studios get exact owned counts.

Net change to the plan below: owned counts come from **ID-only StashDB fetches**
(NOT local tags), and the count pass is still per-subject (can't be skipped) but
fetches ids only. Everything else (lazy bodies, scoped Discover) stands.

## Why

The eager sync downloads **every** owned performer's and studio's full StashDB
filmography on a 12h tick (and a weekly full reconcile). The first full pass
takes ~5.5 minutes and re-downloads ~all scene bodies — to produce two things:

1. **Completion-bar counts** on the Performers/Studios list pages (total / owned
   / missing %, sort-by-missing).
2. **Scene bodies** for the detail pages + Discover.

But you only ever *browse* a handful of those ~1,600 studios. Downloading all of
them to cache bodies you'll never open is the waste. The blocker we assumed —
"we can't show the count without fetching the scenes" — is false: **StashDB
returns a `count` on every query** (we currently throw it away and do
`len(scenes)`). So counts are cheap to get on their own, and bodies can be
fetched lazily on click. That's the redesign.

## Target architecture

### 1. Counts — cheap, eager (the only thing that must be pre-computed)
Replace the body-downloading aggregate pass with a **count pass**: per owned
performer/studio, one StashDB query `per_page: 1, sort: DATE, direction: DESC` →
read `count` (→ `total_stashdb_scenes`) and the first scene's date (→
`last_release_unix`). ~2,400 tiny queries, ~1 min, vs 5+ min of downloads.

`owned_scenes_count` comes from **intersecting the id list with the owned set**
(`FindAllOwnedStashDBSceneIDs`) — same as the eager pass, proven to match (see
Verification above). NOT from local tags. Use a high id cap (~20k) so mega-
studios are exact. So the count pass IS still per-subject (can't be skipped —
owned counts need StashDB's view), but fetches ids only, not bodies.

These three numbers drive the list-page bars exactly as today; nothing else
about `performer_cache` / `studio_cache` changes. Replaces the body-fetch +
`RecomputeAggregates` path — aggregates are written directly from the count pass.

### 2. Scene bodies — lazy, cached on click
`performerFilmography` / `studioFilmography` (`internal/api/server.go`) already
read cache-first with a live fallback (phase 4). Change the fallback to **cache
what it fetches** (`UpsertSceneBatch`) and stamp a per-subject
`bodies_synced_at`. Serve from cache while fresh (TTL, e.g. 24h); on miss/stale,
live-fetch the full filmography, re-cache, serve. No delta needed — a lazy fetch
pulls the whole list. First visit to a subject costs one live fetch (a few
seconds); every visit after is instant until the TTL lapses.

### 3. Discover — scoped recent fetch
Discover needs recent (≤90d) scenes across owned performers, so it can't be
fully lazy (it's a landing page). Keep a small eager pass, but **date-filter** it:
per owned performer, `QueryScenes` with `date >= cutoff` (StashDB
`DateCriterionInput`) instead of the full catalogue. Bounded — most performers
have 0–few scenes in 90 days. Feeds `recent_scene_cache` (+ the body cache).

## What to keep vs unwind (from phases 1–5)
- **Keep**: `stashdb_scene` + `scene_performer` tables, `UpsertScene` /
  `UpsertSceneBatch` / `ScenesForPerformer` / `ScenesForStudio`, the phase-4
  cache-first page serving, the `bodies_synced_at`/watermark column (repurpose
  `scenes_synced_at` as the lazy-TTL stamp).
- **Unwind**: most of `SyncStashDBScenes` (sync.go) — the delta + full-reconcile
  + body-prune machinery exists *only* to make the eager body fetch cheaper. With
  no eager body fetch it's not needed. Replace with the count pass + the scoped
  Discover pass. `QueryScenesSince` (phase 2) can stay (unused, or for a future
  incremental refresh) or be removed. `RecomputeAggregates` /
  `RebuildRecentSceneCache` get replaced by the count pass + scoped Discover.
- **Prune**: lazy bodies need no reconcile-prune; optionally TTL-evict cache rows
  not read in N days (or just leave them — they re-fetch on visit anyway).
- **Remove the now-doubly-dead** `RefreshSceneCache` / `RefreshStudioCache` (already
  dead after phase 5) while in here.

## Build order
1. Owned-count attribution: local sweep → per-subject owned counts (verify
   against current numbers). The make-or-break piece.
2. Count pass: replace the aggregate workers with the `count`-field query;
   wire `last_release` from the same query. Compare bars before/after.
3. Lazy bodies: fallback caches + per-subject TTL; drop the eager body fetch.
4. Scoped Discover pass (date-filtered).
5. Rip out the eager sync machinery; remove dead Refresh*Cache.
6. `POST /refresh/scenes` becomes "recount + refresh Discover" (cheap).

## Verification
- Bars on Performers/Studios match current values (within real drift).
- Opening a never-visited studio: one live StashDB fetch logged, then a second
  visit logs none (cache hit). TTL re-fetch after the window.
- Discover still ~populated (compare scene count to today's ~559/30d).
- 12h tick cost drops from minutes to ~1 min of count queries.
- Tests: owned-count attribution; count-pass aggregate; lazy fallback caches +
  TTL; scoped Discover filter.

## Notes
- Net effect: StashDB load goes from "download everything every 12h" to "cheap
  counts + the bodies you actually open." DB shrinks to viewed subjects + the
  recent window.
- Honest tradeoff vs eager: first visit to a subject is a live fetch (~seconds),
  not instant. Worth it — you browse a few subjects, not 1,600.
