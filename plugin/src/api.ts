// Forage API client. All endpoints live at a single base URL — set
// via the plugin's Settings panel (persisted to localStorage), or
// overridden at build time via the `VITE_FORAGER_URL` env var, or via
// `window.foragerURL` before the SPA bootstraps. Empty default — the
// plugin shows a Settings prompt on first run if nothing is set.

declare global {
  interface Window {
    foragerURL?: string;
  }
}

const DEFAULT_URL = "";
const STORAGE_KEY = "forage.foragerURL";

export function foragerBase(): string {
  // Precedence: window override (for tests) → localStorage (user's
  // own setting via the in-app Settings panel) → build-time env →
  // hardcoded default.
  return (
    window.foragerURL ||
    localStorage.getItem(STORAGE_KEY) ||
    (import.meta.env.VITE_FORAGER_URL as string | undefined) ||
    DEFAULT_URL
  );
}

export function setForagerBase(url: string) {
  const trimmed = url.trim().replace(/\/+$/, "");
  if (trimmed === "") {
    localStorage.removeItem(STORAGE_KEY);
  } else {
    localStorage.setItem(STORAGE_KEY, trimmed);
  }
}

// Detect the mixed-content trap so we can surface a useful error
// message rather than just "NetworkError." If the SPA was loaded
// over HTTPS and the configured forage URL is HTTP, the browser
// will block every fetch silently — no amount of CORS config helps.
export function mixedContentBlocked(): boolean {
  return (
    typeof location !== "undefined" &&
    location.protocol === "https:" &&
    foragerBase().startsWith("http:")
  );
}

// ApiError carries the HTTP status alongside the message so callers (and
// the global 401 handler below) can react to auth failures specifically,
// not just on the error string.
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

// A single app-wide hook the App subscribes to so a 401 from ANY API call
// — even one whose caller swallows the error (the notifications poll) —
// bounces the user back to the login gate. Set once on mount.
let unauthorizedHandler: (() => void) | null = null;
export function setUnauthorizedHandler(fn: (() => void) | null) {
  unauthorizedHandler = fn;
}

// fireUnauthorized notifies the App when a request was rejected for auth.
// Kept separate from throwForStatus so helpers that read the body
// themselves (saveConfig's 422 path) can still signal a 401.
function fireUnauthorized(status: number) {
  if (status === 401) unauthorizedHandler?.();
}

// throwForStatus reads the error body and throws an ApiError, firing the
// global unauthorized hook on 401. Centralises the duplicated
// `if (!r.ok) { … }` blocks so every route surfaces status uniformly.
async function throwForStatus(r: Response): Promise<never> {
  const e = await r.json().catch(() => ({ error: r.statusText }));
  fireUnauthorized(r.status);
  throw new ApiError(r.status, e.error || `HTTP ${r.status}`);
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(foragerBase() + path, {
    signal,
    headers: authHeaders(),
    credentials: "include",
  });
  if (!r.ok) return throwForStatus(r);
  return r.json() as Promise<T>;
}

async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const r = await fetch(foragerBase() + path, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
    credentials: "include",
  });
  if (!r.ok) return throwForStatus(r);
  return r.json() as Promise<T>;
}

// ── Performers list ────────────────────────────────────────────────

export interface Performer {
  stash_id: string;
  stashdb_id?: string;
  name: string;
  aliases?: string[];
  favorite?: boolean;
  scene_count: number;
  // Cached Stash gender enum, what the content-filter chips match on.
  gender?: string;
  // Aggregates populated by the 12h scene-cache refresh. Zero when the
  // refresh hasn't run yet, or when the performer has no StashDB
  // cross-id (so we couldn't query their filmography).
  total_stashdb_scenes: number;
  owned_scenes_count: number;
  last_release_unix: number;
}

export interface PerformersResponse {
  performers: Performer[];
  refreshed_at: number;
  // Deployment-configured content filters (same sets as Discover) as a
  // full name→genders mapping, so the chips filter the already-loaded
  // list in memory, no refetch on toggle. Absent on unconfigured
  // deployments, which keeps the chips hidden.
  filters?: Record<string, string[]>;
}

export type PerformerSort =
  | "scene_count"
  | "name"
  | "last_release"
  | "missing_count";

export function fetchPerformers(opts?: {
  sort?: PerformerSort;
  favoriteOnly?: boolean;
  q?: string;
}): Promise<PerformersResponse> {
  const params = new URLSearchParams();
  if (opts?.sort) params.set("sort", opts.sort);
  if (opts?.favoriteOnly) params.set("favorite_only", "true");
  if (opts?.q) params.set("q", opts.q);
  return get<PerformersResponse>("/performers?" + params.toString());
}

// ── Studios (parity with Performers) ─────────────────────────────────
// A studio's navigation id is its StashDB cross-id (stashdb_id), or a
// synthetic "stash:<local_id>" for studios with no cross-id (which can't be
// queried for a catalogue, so their aggregates stay zero).
export interface Studio {
  stashdb_id: string;
  stash_id?: string;
  name: string;
  aliases?: string[];
  favorite?: boolean;
  scene_count: number;
  total_stashdb_scenes: number;
  owned_scenes_count: number;
  last_release_unix: number;
}

export interface StudiosResponse {
  studios: Studio[];
  refreshed_at: number;
}

// Studios reuse the performer sort vocabulary (same backend sortClause).
export type StudioSort = PerformerSort;

export function fetchStudios(opts?: {
  sort?: StudioSort;
  favoriteOnly?: boolean;
  q?: string;
}): Promise<StudiosResponse> {
  const params = new URLSearchParams();
  if (opts?.sort) params.set("sort", opts.sort);
  if (opts?.favoriteOnly) params.set("favorite_only", "true");
  if (opts?.q) params.set("q", opts.q);
  return get<StudiosResponse>("/studios?" + params.toString());
}

// ── Discover (recent unowned StashDB scenes) ─────────────────────────
//
// Backed by recent_scene_cache on the daemon, which rebuilds every 12h.
// Each scene already includes the local-library performers featured in
// it (denormalised server-side so the UI doesn't need a second lookup
// to render chips).

export interface DiscoverPerformer {
  stash_id: string;
  name: string;
  favorite: boolean;
  // Stats backing the hovercard. Zero when the scene cache hasn't run
  // (or when the performer has no StashDB cross-id).
  scene_count?: number;
  total_stashdb_scenes?: number;
  owned_scenes_count?: number;
  last_release_unix?: number;
}

export interface DiscoverScene {
  stashdb_id: string;
  title?: string;
  release_date?: string;
  release_unix?: number;
  studio_name?: string;
  image_url?: string;
  performers: DiscoverPerformer[];
  watch_status?: string;
}

export interface DiscoverResponse {
  scenes: DiscoverScene[];
  // trending is StashDB's global "what's hot" list, refreshed hourly
  // by the daemon (separate from the 12h performer-filtered scenes).
  // May contain scenes featuring performers you don't have — chips
  // will just be empty for those.
  trending: DiscoverScene[];
  days: number;
  refreshed_at: number;
  trending_refreshed_at: number;
  // Deployment-configured content filters as a full name-to-genders
  // mapping (same shape as /performers); render selection chips only
  // when present, glyphed from each set. Absent on unconfigured
  // (public-default) daemons. Scene filtering stays server-side (flt=).
  filters?: Record<string, string[]>;
}

export function fetchDiscover(opts?: {
  days?: number;
  favoriteOnly?: boolean;
  limit?: number;
  trendingLimit?: number;
  filter?: string; // deployment-configured content filter name
}): Promise<DiscoverResponse> {
  const params = new URLSearchParams();
  if (opts?.days != null) params.set("days", String(opts.days));
  if (opts?.favoriteOnly) params.set("favorite_only", "true");
  if (opts?.filter) params.set("flt", opts.filter);
  if (opts?.limit != null) params.set("limit", String(opts.limit));
  if (opts?.trendingLimit != null)
    params.set("trending_limit", String(opts.trendingLimit));
  const qs = params.toString();
  return get<DiscoverResponse>("/discover" + (qs ? "?" + qs : ""));
}

// ── Missing scenes ─────────────────────────────────────────────────

export interface MissingPerformer {
  name: string;
  as?: string;
  // Alternate spellings from the user's local Stash record — a tracker may
  // have listed the release under one of these. Populated for scene-release
  // alias retries; absent elsewhere.
  aliases?: string[];
}

export interface MissingScene {
  stashdb_id: string;
  title: string;
  date?: string;
  studio?: string;
  studio_id?: string;
  performers: MissingPerformer[];
  url?: string;
  image_url?: string;
  // In-flight grab status (queued/downloading/completed/placed/scanned)
  // when this scene has been grabbed but isn't in the library yet. Empty
  // when nothing is in flight for it.
  grab_status?: string;
  // Watch state ("watching" | "available") when the user is tracking this
  // scene; empty otherwise.
  watch_status?: string;
}

// OwnedScene is a scene already in the library, annotated with the current
// best copy's quality so the performer page can flag upgrade candidates.
export interface OwnedScene {
  stashdb_id: string;
  title: string;
  date?: string;
  studio?: string;
  studio_id?: string;
  performers: MissingPerformer[];
  url?: string;
  image_url?: string;
  // Current best copy: resolution label ("480p"/"1080p"/"2160p"), raw pixel
  // height (sort key), and file size in bytes. Empty/zero if Stash had no
  // dimensions.
  resolution?: string;
  height?: number;
  size?: number;
  // In-flight upgrade grab status for this scene, if any.
  grab_status?: string;
}

// SceneCopyView is one local Stash scene carrying a StashDB cross-id — a
// "copy" in the duplicates view. Deleting it removes that whole scene + file.
export interface SceneCopyView {
  scene_id: string;
  path?: string;
  resolution?: string;
  height?: number;
  size?: number;
}

// DuplicateScene is a scene you hold 2+ separate library copies of. copies is
// sorted best-resolution-first.
export interface DuplicateScene {
  stashdb_id: string;
  title: string;
  date?: string;
  studio?: string;
  image_url?: string;
  copies: SceneCopyView[];
}

// Subject is who/what a missing-scenes page is about — a performer or a
// studio. They share the whole owned/missing/dupes pipeline; only the StashDB
// query and grab placement differ.
export interface Subject {
  kind: "performer" | "studio";
  local_id: string;
  stashdb_id: string;
  name: string;
}

export interface MissingResponse {
  subject: Subject;
  // Retained for back-compat (performer subject only); new code reads subject.
  performer?: {
    local_id: string;
    stashdb_id: string;
    name: string;
  };
  total_scenes: number;
  owned_count: number;
  missing: MissingScene[];
  owned: OwnedScene[];
  duplicates: DuplicateScene[];
  // Local scenes tagged with this subject that have NO StashDB id — invisible to
  // the owned/missing math (a scene you hold but never identified can otherwise
  // read as "missing"). Common with amateur creators.
  unidentified_local: number;
}

// destroyScene deletes one local Stash scene (and its file) by LOCAL scene id
// — the duplicates-cleanup action. Irreversible; caller confirms intent.
export function destroyScene(sceneId: string): Promise<{ ok: boolean }> {
  return postJSON(`/scenes/${encodeURIComponent(sceneId)}/destroy`, {});
}

// fetchMissing loads the gap analysis for a performer or a studio. The studio
// id is the studio_cache key (its StashDB cross-id).
export function fetchMissing(
  subject: { performer: string } | { studio: string },
): Promise<MissingResponse> {
  const params = new URLSearchParams();
  if ("performer" in subject) params.set("performer", subject.performer);
  else params.set("studio", subject.studio);
  return get<MissingResponse>("/missing-scenes?" + params.toString());
}

// ── Scene releases ─────────────────────────────────────────────────

export interface SceneRelease {
  title: string;
  indexer: string;
  protocol: "torrent" | "usenet";
  size: number;
  popularity: number;
  seeders: number;
  grabs: number;
  publish_date: string;
  info_url: string;
  download_url: string;
  verified: boolean;
  confidence: number;
  // Failure history for this exact release (title+indexer+size): grabs
  // of it that died for a content-side reason (missing articles,
  // unrepairable, corrupt, libtorrent-declined). A badge, so a release
  // that already died 8 times reads as dead before the 9th Grab click.
  failed_count?: number;
  failed_reason?: string;
  // Populated only when the release is NOT verified because the
  // matcher thinks it's a different scene. Lets the UI warn the user
  // that grabbing this would not get them the scene they're viewing.
  best_match_id?: string;
  best_match_title?: string;
  best_match_conf?: number;
  // Matcher's per-component breakdown for the viewed scene against this
  // release (e.g. "performers: 2/2", "title: 0.43"). Drives the "why?"
  // expander. Absent when the viewed scene wasn't a candidate.
  reasons?: string[];
  // Release-preference score (sum of matched rules), reject flag, and the
  // per-rule breakdown. Drives ranking + the score chip.
  score?: number;
  rejected?: boolean;
  score_hits?: { label: string; points: number; reject?: boolean }[];
}

export interface SceneReleasesResponse {
  scene: {
    stashdb_id: string;
    title: string;
    date?: string;
    studio?: string;
    image_url?: string;
    performers: MissingPerformer[];
  };
  releases: SceneRelease[];
}

// ── Images ─────────────────────────────────────────────────────────
//
// forage renders performer portraits + scene screenshots through the
// daemon's own image proxy (`/img/...`), which fetches from Stash
// server-side with the stored API key. So image URLs are just
// foragerBase()-relative — same-origin when the daemon serves the app
// standalone. The browser never needs Stash credentials. On the Vite dev
// server with no daemon configured there's nothing to proxy, so images
// fall through to a placeholder.

export function imageBase(): string | null {
  const base = foragerBase();
  if (base) return base; // explicit daemon URL configured
  // base === "" → same-origin. Correct when the daemon serves us; wrong on
  // the dev server (localhost:5173), where no daemon lives at this origin.
  if (
    typeof location !== "undefined" &&
    (location.hostname === "localhost" || location.hostname === "127.0.0.1")
  ) {
    return null;
  }
  return ""; // same-origin daemon
}

export function performerImageURL(localStashID: string): string | null {
  const base = imageBase();
  if (base === null) return null;
  return `${base}/img/performer/${encodeURIComponent(localStashID)}`;
}

// studioImageURL proxies a studio's image (logo/banner) from Stash by its
// LOCAL Stash studio id. null when images can't be proxied (dev cross-origin).
export function studioImageURL(localStashID: string): string | null {
  const base = imageBase();
  if (base === null) return null;
  return `${base}/img/studio/${encodeURIComponent(localStashID)}`;
}

// proxiedImageURL resolves a daemon-relative image path (e.g. a grab
// detail's `/img/scene/{id}/screenshot`) against the daemon base. Leaves
// absolute URLs untouched (legacy data / StashDB CDN); returns null when
// there's no daemon to proxy through.
export function proxiedImageURL(path?: string | null): string | null {
  if (!path) return null;
  if (/^https?:\/\//i.test(path)) return path;
  const base = imageBase();
  if (base === null) return null;
  return `${base}${path}`;
}

// establishSession posts the stored admin token so the daemon sets the
// forage_token cookie — required for <img> requests (portraits, screenshots)
// to authenticate, since image loads can't carry the bearer header. The
// daemon replies ok/required:false when no token is configured, so this is
// safe to call unconditionally. Call on boot and whenever the token changes.
export async function establishSession(): Promise<void> {
  try {
    await fetch(foragerBase() + "/session", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token: adminToken() }),
      credentials: "include",
    });
  } catch {
    // Best-effort — images fall back to placeholders if this fails.
  }
}

// login posts username+password to the daemon's POST /login. On success
// the daemon sets the forage_token cookie (a server-side session id, NOT
// the password) and we're authenticated by that cookie thereafter — no
// localStorage involved. Throws ApiError(401) on bad credentials, which
// the Login view renders as "incorrect username or password".
export async function login(username: string, password: string): Promise<void> {
  const r = await fetch(foragerBase() + "/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password }),
    credentials: "include",
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new ApiError(r.status, e.error || `HTTP ${r.status}`);
  }
}

// verifyToken probes a gated endpoint to confirm the stored token (Bearer
// header + cookie) is currently accepted. Returns true on 200, false on a
// 401 (or unreachable). Deliberately does NOT fire the global unauthorized
// handler — the login gate and the boot probe handle the false case
// themselves, so a verification miss mustn't recursively bounce the app.
export async function verifyToken(): Promise<boolean> {
  try {
    const r = await fetch(foragerBase() + "/notifications", {
      headers: authHeaders(),
      credentials: "include",
    });
    return r.ok;
  } catch {
    return false;
  }
}

// clearSession asks the daemon to expire the forage_token cookie (it's
// HttpOnly, so client JS can't). Best-effort half of logout; the caller
// also drops the stored bearer token via setAdminToken("").
export async function clearSession(): Promise<void> {
  try {
    await fetch(foragerBase() + "/session", {
      method: "DELETE",
      credentials: "include",
    });
  } catch {
    // Best-effort — the bearer token is cleared client-side regardless.
  }
}

export function fetchSceneReleases(
  stashDBID: string,
  opts?: {
    performer?: string;
    alias?: string;
    lean?: boolean;
    signal?: AbortSignal;
  },
): Promise<SceneReleasesResponse> {
  const q = new URLSearchParams();
  if (opts?.performer) q.set("performer", opts.performer);
  if (opts?.alias) q.set("alias", opts.alias);
  if (opts?.lean) q.set("lean", "1");
  const qs = q.toString();
  return get<SceneReleasesResponse>(
    `/scenes/${encodeURIComponent(stashDBID)}/releases${qs ? "?" + qs : ""}`,
    opts?.signal,
  );
}

// ── Performer packs ─────────────────────────────────────────────────
//
// A pack is a single torrent holding many of a performer's scenes. The
// daemon finds them by searching the performer name on torrent indexers
// and parsing each candidate's .torrent for an authoritative video
// count. Grabbing a pack downloads the whole thing, then forage places
// every file, identifies them against StashDB, and dedups any the
// library already had.

export interface Pack {
  title: string;
  indexer: string;
  protocol: "torrent" | "usenet";
  size: number;
  // Video count parsed from the release title; 0 when the title states
  // none. It's an estimate, confirmed only once the pack is grabbed and
  // scanned (browsing never downloads .torrents).
  video_count: number;
  seeders: number;
  grabs: number;
  popularity: number;
  publish_date: string;
  info_url: string;
  download_url: string;
}

export interface PacksResponse {
  performer: { stash_id: string; name: string };
  packs: Pack[];
}

export function fetchPacks(
  performerId: string,
  signal?: AbortSignal,
): Promise<PacksResponse> {
  return get<PacksResponse>(
    `/performers/${encodeURIComponent(performerId)}/packs`,
    signal,
  );
}

// ── Grab ───────────────────────────────────────────────────────────

export interface GrabRequest {
  download_url: string;
  release_title: string;
  release_size?: number;
  release_indexer?: string;
  protocol: "torrent" | "usenet";
  scene_id?: string;
  confidence?: number;
  // Sent through to forage's placer — the folder under <library_root>
  // the finished file will land in. Optional; defaults to "Unsorted"
  // server-side when missing.
  performer_name?: string;
  // "pack" for a performer-pack grab (one torrent → many scenes). When
  // set, video_count carries the parsed count so the daemon knows how
  // many scenes to drive identify toward.
  kind?: "single" | "pack";
  video_count?: number;
  // Bypass the daemon's disk-space preflight (user chose "grab anyway").
  force?: boolean;
}

export interface GrabResponse {
  ok: boolean;
  client?: string;
  category?: string;
  grab_id?: number;
  client_id?: string;
}

export function postGrab(req: GrabRequest): Promise<GrabResponse> {
  return postJSON<GrabResponse>("/grab", req);
}

// grabTorrentFile uploads a .torrent the user supplied directly (e.g.
// from a private tracker forage can't search). forage adds it to qBit
// and runs the normal place→scan→identify pipeline; `name` is the
// library folder to place into (blank → "(manual)" server-side). Pack
// vs single is auto-detected from the parsed video count.
export async function grabTorrentFile(
  file: File,
  name: string,
): Promise<GrabResponse> {
  const fd = new FormData();
  fd.append("torrent", file);
  if (name) fd.append("name", name);
  const r = await fetch(foragerBase() + "/grab/torrent", {
    method: "POST",
    headers: authHeaders(),
    credentials: "include",
    body: fd,
  });
  if (!r.ok) return throwForStatus(r);
  return r.json() as Promise<GrabResponse>;
}

export interface SuggestedPerformer {
  stash_id: string;
  name: string;
  scene_count: number;
  favorite: boolean;
}

// TorrentInspect is what /grab/torrent/inspect returns: the torrent's real
// internal name + size/counts (not the opaque download filename) plus
// performers matched from that name, so the user can pick a folder before
// committing the download.
export interface TorrentInspect {
  name: string;
  total_size: number;
  file_count: number;
  video_count: number;
  kind: "pack" | "single";
  suggested_performers: SuggestedPerformer[];
}

// inspectTorrentFile parses a .torrent without grabbing it.
export async function inspectTorrentFile(file: File): Promise<TorrentInspect> {
  const fd = new FormData();
  fd.append("torrent", file);
  const r = await fetch(foragerBase() + "/grab/torrent/inspect", {
    method: "POST",
    headers: authHeaders(),
    credentials: "include",
    body: fd,
  });
  if (!r.ok) return throwForStatus(r);
  return r.json() as Promise<TorrentInspect>;
}

// ── Grabs list ────────────────────────────────────────────────────
//
// Mirrors internal/api/grabs.go's grabOut shape. Status values follow
// the poller's state machine:
//   queued → downloading → completed → placed → confirmed
//                                            ↘ mismatched / orphaned
//                                ↘ failed
// "queued", "downloading", "completed", "placed" are non-terminal —
// the poller keeps advancing them. The others are terminal.

export type GrabStatus =
  | "queued"
  | "deferred"
  | "downloading"
  | "completed"
  | "placed"
  | "scanned"
  | "tagging"
  | "distributing"
  | "confirmed"
  | "mismatched"
  | "orphaned"
  | "failed";

// GrabFilter is what the Grabs list can filter BY, which is a superset of the
// statuses a grab can be IN. "unmatched" is a pseudo-status the daemon
// resolves server-side: a grab that settled 'confirmed' without ever being
// linked to a StashDB scene. It splits the confirmed rows rather than adding a
// status, so totals.confirmed + totals.unmatched still covers them all.
export type GrabFilter = GrabStatus | "any" | "unmatched";

export interface Grab {
  id: number;
  predicted_stashdb_id?: string;
  predicted_confidence?: number;
  actual_stashdb_id?: string;
  // The StashDB scene's real title — present only for grabs the daemon
  // grouped (2+ attempts at one scene), so the group header can show it
  // instead of a bare id. Empty/absent otherwise.
  scene_title?: string;
  release_title: string;
  release_size?: number;
  release_indexer?: string;
  download_url?: string;
  client?: string;
  client_id?: string;
  client_name?: string;
  category?: string;
  status: GrabStatus;
  // True when a torrent grab has been "downloading" with no progress
  // for a while — the UI badges it so you can abandon + pick another.
  stalled?: boolean;
  // True when forage adopted this from a torrent added to qBit directly
  // (forager category), rather than grabbing it through forage.
  adopted?: boolean;
  // True when the download finished but placement into the library keeps
  // failing (permission / mount / path issue).
  place_failing?: boolean;
  reason?: string;
  // Deferred-retry state (status "deferred"): failed add attempts so far
  // and the unix time the daemon will automatically re-drive the add.
  attempts?: number;
  next_retry_at?: number;
  max_attempts?: number;
  // Why the grab is deferred: "indexer" (fetch failed; failover may
  // rescue) or "client" (download client unreachable; retry on recovery).
  fail_kind?: string;
  performer_name?: string;
  placed_path?: string;
  place_error?: string;
  grabbed_at: number;
  updated_at: number;
  completed_at?: number;
  placed_at?: number;
  confirmed_at?: number;
  // Live download state, present only while downloading/queued.
  progress?: { percent: number; speed_bps?: number; eta_secs?: number };
  // Pack grabs (one torrent → many scenes). kind is "pack"; the
  // counters track identify + dedup progress across the pack's files.
  kind?: "single" | "pack";
  pack_files?: number;
  pack_identified?: number;
  pack_deduped?: number;
  // Count of unresolved review-mode dedup items (PackDedupKeep="review") —
  // scenes the pack delivered that the library already had. The card badges
  // this and loads the per-scene compare from the detail endpoint.
  pending_duplicates?: number;
}

export interface GrabsResponse {
  grabs: Grab[];
  // Keyed by GrabFilter, not GrabStatus: the daemon reports "unmatched"
  // alongside the real statuses, with those rows deducted from "confirmed" so
  // summing every value still gives the grand total.
  totals: Partial<Record<GrabFilter, number>>;
  // How many grabs match the current status+q filter across the WHOLE table
  // (not just this page). Lets the view show a result count and know when
  // "load more" has reached the end. Absent on older daemons → treat as
  // unknown (fall back to page-length heuristics).
  match_total?: number;
}

// adoptDownloads force-adopts forage-category downloads the user added to a
// client manually — qBit torrents immediately (bypassing the 5-min adoption
// grace), completed SAB jobs from history. Returns how many grabs were
// created.
export function adoptDownloads(): Promise<{
  ok: boolean;
  adopted: number;
  // Torrents skipped only for being added in the last 90s — the periodic
  // poller picks them up automatically within ~5 min.
  skippedRecent: number;
}> {
  return postJSON("/grabs/adopt", {});
}

// retryFailedGrabs re-queues every failed grab that has a download URL (bulk
// recovery after a collection-job batch). Returns retried + skipped counts.
export function retryFailedGrabs(): Promise<{
  ok: boolean;
  retried: number;
  skipped: number;
}> {
  return postJSON("/grabs/retry-failed", {});
}

export function fetchGrabs(opts?: {
  status?: GrabFilter;
  q?: string;
  limit?: number;
  offset?: number;
}): Promise<GrabsResponse> {
  const params = new URLSearchParams();
  if (opts?.status && opts.status !== "any") params.set("status", opts.status);
  if (opts?.q && opts.q.trim()) params.set("q", opts.q.trim());
  if (opts?.limit) params.set("limit", String(opts.limit));
  if (opts?.offset) params.set("offset", String(opts.offset));
  const qs = params.toString();
  return get<GrabsResponse>("/grabs" + (qs ? "?" + qs : ""));
}

// GrabDetail enriches the expanded grab card: StashDB scene metadata
// plus a deep-link into the user's local Stash when the file has
// landed there.
export interface GrabDetail {
  stashdb_id?: string;
  title?: string;
  date?: string;
  studio?: string;
  image_url?: string;
  performer_image_url?: string;
  local_scene_image_url?: string;
  performers: { name: string; as?: string }[];
  local_scene_id?: string;
  stash_scene_url?: string;
  // The StashDB cross-id actually on the placed scene in Stash — the truth
  // for "is this identified?", which can outrun the grab's actual_stashdb_id
  // (an adopted single settles before identify lands, or you match it by
  // hand later). The identify hero trusts this over the grab field.
  local_stashdb_id?: string;
  // Ranked local-performer guesses from the release title — the one-click
  // options for reassigning a mis-filed / Unsorted grab. Absent for packs.
  performer_suggestions?: {
    stash_id: string;
    name: string;
    scene_count: number;
    favorite: boolean;
  }[];
  // Pending review-mode duplicates: scenes this pack delivered that the
  // library already had. Each compares the pack's copy against the
  // pre-existing one(s) so the user keeps the better file.
  duplicates?: DuplicateReview[];
}

// SceneCopy is one physical copy of a scene — height buckets to a resolution
// label, size shows the file weight, path disambiguates where it lives.
export interface SceneCopy {
  scene_id: string;
  title?: string;
  path?: string;
  size?: number;
  height?: number;
}

export interface DuplicateReview {
  id: number;
  stashdb_id: string;
  scene_title?: string;
  pack: SceneCopy;
  existing: SceneCopy[];
}

export function fetchGrabDetail(id: number): Promise<GrabDetail> {
  return get<GrabDetail>(`/grabs/${id}/detail`);
}

// resolveDuplicate applies a keep decision to one pending review item.
// "existing" drops the pack's copy, "pack" drops the pre-existing one(s),
// "both" keeps everything (dismiss). The destroy runs server-side.
export function resolveDuplicate(
  id: number,
  keep: "existing" | "pack" | "both",
): Promise<{ ok: boolean; resolution: string; removed?: string[]; errors?: string[] }> {
  return postJSON(`/duplicates/${id}/resolve`, { keep });
}

export interface MatchResult {
  ok: boolean;
  stashdb_id: string;
  title?: string;
  performers_applied?: number;
  studio_applied?: boolean;
}

// matchGrab manually links a grab's placed scene to a StashDB scene and
// applies that scene's metadata (title/date/urls + cross-id + the
// performers/studio already in your library). Omit stashdbId to apply
// the grab's own prediction; pass a UUID or stashdb.org/scenes/<id> URL
// to match an explicit scene.
export function matchGrab(id: number, stashdbId?: string): Promise<MatchResult> {
  return postJSON<MatchResult>(
    `/grabs/${id}/match`,
    stashdbId ? { stashdb_id: stashdbId } : {},
  );
}

// A ranked StashDB scene candidate for a grab phash couldn't link. The
// matcher's best guesses against the grab's own name, each scored with the
// tokens that matched (reasons) so the file can be picked by eye when the
// confidence number alone is inconclusive. See GET /grabs/{id}/match-candidates.
export interface MatchCandidate {
  scene_id: string;
  title: string;
  studio?: string;
  date?: string;
  performers?: string[];
  confidence: number;
  tracks: string[];
  reasons: string[];
  url?: string;
  image?: string; // StashDB cover for the pick-list thumbnail
}

// fetchGrabMatchCandidates runs the matcher against a grab's own name
// (release title -> placed basename -> client name) and returns ranked
// StashDB scenes to choose from. Read-only: it suggests, matchGrab applies.
export function fetchGrabMatchCandidates(
  id: number,
): Promise<{ candidates: MatchCandidate[] }> {
  return get<{ candidates: MatchCandidate[] }>(`/grabs/${id}/match-candidates`);
}

// retryGrab re-attempts a failed grab from its stored download URL.
export function retryGrab(id: number): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>(`/grabs/${id}/retry`, {});
}

// setGrabPerformer reassigns a grab's performer folder and re-files the
// (still-seeding) download into <library>/<performer>/, removing the old
// library link. The fix for an Unsorted / mis-identified adopted grab.
export function setGrabPerformer(
  id: number,
  performerName: string,
): Promise<{ ok: boolean; performer_name: string; placed_path: string }> {
  return postJSON(`/grabs/${id}/performer`, { performer_name: performerName });
}

// One taggable scene in a pack: a placed file Stash scanned but could NOT
// cross-id against StashDB (amateur content). image_url is the daemon-relative
// /img/scene/{id}/screenshot (works for unidentified locals).
export interface PackScene {
  scene_id: string;
  title?: string;
  image_url: string;
}
export interface PackScenes {
  performer_name: string;
  performer_local_id?: string;
  performer_resolvable: boolean;
  scenes: PackScene[];
}

// fetchPackScenes lists a pack grab's UNIDENTIFIED scenes (with covers) + the
// performer it's filed under — the "tag amateur scenes with the performer"
// reviewer on the pack grab card.
export function fetchPackScenes(grabId: number): Promise<PackScenes> {
  return get<PackScenes>(`/grabs/${grabId}/pack-scenes`);
}

// applyPackPerformer adds the pack's performer to the selected scenes, additively
// (existing performers preserved). The server re-validates each id against the
// pack's unidentified set, so it can never touch identified or non-pack scenes.
export function applyPackPerformer(
  grabId: number,
  sceneIds: string[],
): Promise<{ ok: boolean; applied: number }> {
  return postJSON(`/grabs/${grabId}/apply-performer`, { scene_ids: sceneIds });
}

// GrabDeletePreview is exactly the plan DELETE /grabs/{id} will execute,
// resolved server-side: which Stash scenes (with every file path) go, which
// were refused and why, what the disk sweep removes, and what happens at the
// download client. Shown under the armed delete button so confirming is
// informed consent, not a reflex.
export interface DeletePreviewFile {
  path: string;
  size?: number;
}
export interface DeletePreviewScene {
  scene_id: string;
  title?: string;
  files: DeletePreviewFile[];
}
export interface DeletePreviewKept {
  target: DeletePreviewScene;
  reason: string;
}
export interface GrabDeletePreview {
  // Present when deletions are recoverable: files move to forage's trash
  // and are kept for kept_days before the sweep unlinks them. Absent =
  // permanent deletion (FORAGER_TRASH_TTL=0 escape hatch, or no library).
  trash?: { enabled: boolean; kept_days: number };
  // Nullable defensively even though the daemon sends [] — an older daemon
  // (or a proxy rewriting empty arrays) must not crash the delete flow.
  scenes: DeletePreviewScene[] | null;
  kept?: DeletePreviewKept[];
  disk?: string[];
  client?: string;
  notes?: string[];
  grab_record: boolean;
}

export function fetchGrabDeletePreview(id: number): Promise<GrabDeletePreview> {
  return get<GrabDeletePreview>(`/grabs/${id}/delete-preview`);
}

export interface DeleteGrabResult {
  ok: boolean;
  removed: string[];
  errors?: string[];
}

export async function deleteGrab(id: number): Promise<DeleteGrabResult> {
  const r = await fetch(foragerBase() + `/grabs/${id}`, {
    method: "DELETE",
    headers: authHeaders(),
    credentials: "include",
  });
  if (!r.ok) return throwForStatus(r);
  return r.json() as Promise<DeleteGrabResult>;
}

// Non-terminal statuses — used by the GrabsList view to decide poll
// cadence: if any active grabs exist, poll fast; otherwise slow.
// `scanned` is non-terminal: the daemon keeps re-checking until
// Stash's identify attaches a StashDB cross-id and we transition to
// confirmed/mismatched.
export const ACTIVE_STATUSES: GrabStatus[] = [
  "queued",
  "downloading",
  "completed",
  "placed",
  "scanned",
  "tagging",
  "distributing",
];

export function isActiveStatus(s: GrabStatus): boolean {
  return ACTIVE_STATUSES.includes(s);
}

// ── Subscriptions (permanent performer/studio watches) ────────────

export interface Subscription {
  stashdb_id: string;
  kind: "performer" | "studio";
  name: string;
  // LOCAL Stash id of the subject: navigation target for the card, and
  // the id the daemon's image proxy serves portraits by.
  local_id?: string;
  image_url?: string;
  auto_grab: boolean;
  created_at: number;
  last_checked: number;
  // Rail badges: watches created since the card was last opened, and
  // watches sitting available (grabbable) right now.
  new_count: number;
  ready_count: number;
}

export function fetchSubscriptions(): Promise<{ subscriptions: Subscription[] }> {
  return get<{ subscriptions: Subscription[] }>("/subscriptions");
}

export function subscribe(sub: {
  // The subject's StashDB cross-id (subject pages have it on
  // data.subject.stashdb_id); the loop matches scenes by it, so a
  // local id here would never fire. The server re-resolves defensively.
  stashdb_id: string;
  kind: "performer" | "studio";
  name: string;
  local_id?: string;
  image_url?: string;
}): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>("/subscriptions", sub);
}

export async function unsubscribe(stashdbID: string): Promise<{ ok: boolean }> {
  const r = await fetch(
    foragerBase() + `/subscriptions/${encodeURIComponent(stashdbID)}`,
    { method: "DELETE", headers: authHeaders(), credentials: "include" },
  );
  if (!r.ok) return throwForStatus(r);
  return r.json() as Promise<{ ok: boolean }>;
}

export function setSubscriptionAutoGrab(
  stashdbID: string,
  enabled: boolean,
): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>(
    `/subscriptions/${encodeURIComponent(stashdbID)}/auto-grab`,
    { enabled },
  );
}

export function createUpgradeWatches(req: {
  kind: "performer" | "studio";
  stashdb_id: string;
  name: string;
  cutoff: number; // height: watch owned scenes strictly below this
}): Promise<{ ok: boolean; created: number; already_watched: number; at_or_above_cutoff: number }> {
  return postJSON("/watches/upgrade", req);
}

export function markSubscriptionSeen(stashdbID: string): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>(
    `/subscriptions/${encodeURIComponent(stashdbID)}/seen`,
    {},
  );
}

// ── Health (used to know what clients are configured) ─────────────

export interface Health {
  ok: boolean;
  version: string;
  performerCount: number;
  studioCount: number;
  stashConfigured: boolean;
  stashdbConfigured: boolean;
  prowlarrConfigured: boolean;
  qbitConfigured: boolean;
  qbitCategory: string;
  // True when a configured qBit answered its last reachability probe.
  // Optimistically true until the first probe lands and whenever qBit
  // isn't configured, so it only reads false on a confirmed outage.
  qbitReachable: boolean;
  sabConfigured: boolean;
  sabCategory: string;
  // SAB's equivalent of qbitReachable.
  sabReachable: boolean;
  // Human-readable reachability failures for configured-but-unreachable
  // download clients, e.g. "qbit: dial tcp 127.0.0.1:8083: connection
  // refused". Empty when every configured client is reachable.
  clientErrors: string[];
  placerConfigured: boolean;
  unconfigured: boolean;
  // True when a credential is configured (password OR API key) and the UI
  // must show a login gate.
  adminAuthRequired: boolean;
  // True when a username+password is set — the UI shows the password login
  // form; false (with adminAuthRequired) means a token-only daemon, so the
  // UI falls back to the API-key field.
  passwordSet: boolean;
  // The most recent recovered background panic, if any ever happened:
  // unix seconds + which loop. The full stack lives behind auth in /diag.
  lastPanic?: { at: number; in: string };
  // Which torrent backend grabs ride: "qbit", or "" when none is
  // (no torrent path configured).
  torrentBackend?: string;
}

export function fetchHealth(): Promise<Health> {
  return get<Health>("/healthz");
}

// ManagedProwlarrStatus mirrors the daemon's managed.Status: the install/
// supervision state machine for the Prowlarr instance forage can install
// and run itself.
export interface ManagedProwlarrStatus {
  supported: boolean;
  installed: boolean;
  state:
    | "absent"
    | "downloading"
    | "installing"
    | "starting"
    | "running"
    | "stopped"
    | "error";
  detail?: string;
  doneBytes?: number;
  totalBytes?: number;
  version?: string;
  url?: string;
}

// ── Indexer catalog (Prowlarr definitions, adult-capable only) ───────

export interface CatalogField {
  name: string;
  label: string;
  password: boolean;
}

export interface CatalogEntry {
  id: string;
  name: string;
  protocol: string;
  privacy: string;
  description?: string;
  fields: CatalogField[];
  complex: boolean;
  added: boolean;
}

export function fetchIndexerCatalog(): Promise<{ catalog: CatalogEntry[] }> {
  return get<{ catalog: CatalogEntry[] }>("/indexer-catalog");
}

// addCatalogIndexer tests the definition against the real site, then
// creates it in Prowlarr. Throws with Prowlarr's validation message on a
// failed test.
export function addCatalogIndexer(
  id: string,
  fields: Record<string, string>,
): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>("/indexer-catalog/add", { id, fields });
}

// prowlarrProxyHref is the managed instance's UI through forage's
// authenticated reverse proxy — the one URL a remote browser can reach it
// at (the instance itself is localhost-only).
export function prowlarrProxyHref(): string {
  return foragerBase() + "/prowlarr/";
}

export function fetchManagedProwlarr(): Promise<ManagedProwlarrStatus> {
  return get<ManagedProwlarrStatus>("/managed/prowlarr");
}

export function installManagedProwlarr(): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>("/managed/prowlarr/install", {});
}

// IndexerInfo is one configured Prowlarr indexer, as the friendly
// release-prefs editor needs it: a name to rank/block, a protocol badge,
// and whether it's enabled.
export interface IndexerInfo {
  name: string;
  protocol: string; // "torrent" | "usenet"
  enabled: boolean;
  // "public" | "private" | "semiPrivate" — the seeding section badges
  // private trackers, the ones per-indexer overrides exist to protect.
  privacy?: string;
}

// fetchIndexers lists the user's Prowlarr indexers for the friendly
// release-ranking UI. Returns an empty list when Prowlarr is unconfigured
// (the daemon answers 200 + [] in that case, so the UI hides the section).
export async function fetchIndexers(): Promise<IndexerInfo[]> {
  const r = await get<{ indexers: IndexerInfo[] }>("/indexers");
  return r.indexers ?? [];
}

// ── Daemon config (UI-editable via /config) ───────────────────────────

export type FieldSource = "json" | "env" | "default";

export interface ConfigField {
  // value is omitted (empty) for masked secrets; UI shows a placeholder
  // when hasSecret is true.
  value: string | number[] | boolean | null;
  source: FieldSource;
  hasSecret?: boolean;
}

export interface ConfigFieldsResponse {
  fields: Record<string, ConfigField>;
  sectionConfigured: Record<string, boolean>;
}

// ConfigPatch matches the Go configstore.Patch shape. Every field is
// optional — undefined means "leave unchanged"; explicit empty-string
// clears the field, falling back to env or default.
// ReleaseRule mirrors the daemon's scoring.Rule. A release's score is the
// sum of matched rules' points; a matched reject rule disqualifies it.
// `on` selects the field the pattern matches: the release title (where
// resolution lives) or the indexer/source name.
export interface ReleaseRule {
  label: string;
  on?: "title" | "indexer" | "protocol";
  pattern: string;
  points: number;
  reject?: boolean;
}

export interface ConfigPatch {
  stashUrl?: string;
  stashApiKey?: string;
  stashdbUrl?: string;
  stashdbApiKey?: string;
  prowlarrUrl?: string;
  prowlarrApiKey?: string;
  prowlarrCategories?: number[];
  qbitUrl?: string;
  qbitUsername?: string;
  qbitPassword?: string;
  qbitCategory?: string;
  sabUrl?: string;
  sabApiKey?: string;
  sabCategory?: string;
  // Where the download clients put finished files. forage creates the
  // forage category in qBit/SAB pointing here, so the client never has to
  // be configured by hand.
  downloadRoot?: string;
  // Seeding management (see the Settings Seeding section): retire limits as
  // Go duration / ratio strings, plus the per-indexer overrides JSON the
  // editor writes so users never hand-author it.
  trashTtl?: string;
  seedMaxAge?: string;
  seedRatio?: string;
  seedOverrides?: string;
  libraryRoot?: string;
  stashPathMapping?: string;
  sabDeleteAfterPlace?: boolean;
  // "existing" (keep your copy, drop the pack dup), "pack" (keep the
  // pack copy, drop yours), or "both" (no dedup).
  packDedupKeep?: string;
  // Release-scoring rules as a JSON array string (ReleaseRule[]). Empty =
  // built-in defaults.
  releaseRules?: string;
  // Friendly (no-typing) release-ranking prefs as a JSON string
  // (ReleasePrefs). Client-owned; the daemon stores it but never interprets
  // it — it's compiled into releaseRules client-side.
  releasePrefs?: string;
  // True once releaseRules was hand-tuned in the advanced editor, so the
  // client stops auto-recompiling them from releasePrefs.
  releaseAdvanced?: boolean;
  // StashDB tag names whose scenes are dropped from the missing-scenes
  // gap analysis (case-insensitive). Empty = no filtering.
  excludedSceneTags?: string[];
  pollInterval?: string;
  orphanAfter?: string;
  cacheRefresh?: string;
  allowedOrigin?: string;
  // API key gating programmatic clients. Empty clears it. Stored
  // server-side in config.json; the plugin adopts it into localStorage
  // after a successful save so it keeps authenticating.
  adminToken?: string;
  // Web-UI login name. Paired with `password`.
  username?: string;
  // Write-only plaintext password — the daemon bcrypt-hashes it; the hash
  // never round-trips back. Empty string clears it (turns password login
  // off). Omit to leave unchanged.
  password?: string;
  // Telegram notification sink: bot token (secret, masked like API keys)
  // + chat id. Both required for Telegram pushes to activate.
  telegramBotToken?: string;
  telegramChatId?: string;
  // Generic notification webhook: receives {"event","message","ts"} JSON
  // per event batch. Empty = off.
  notifyWebhookUrl?: string;
  // Stash base URL notification links point at (the address the user's
  // devices can reach). Empty = links fall back to stashUrl.
  stashPublicUrl?: string;
}

export interface ProbeResult {
  ok: boolean;
  // Written for a human: what went wrong and what to check.
  message?: string;
  // The raw client error behind it. Shown on demand, so a failure stays
  // diagnosable without the setup screen reading like a stack trace.
  detail?: string;
}

export interface SaveConfigResponse {
  ok: boolean;
  results: Record<string, ProbeResult>;
  // Per-client outcome of the automatic forage-category creation the save
  // performs ("ready" | "failed: …"), present when a download folder is
  // set and a client is configured.
  categories?: Record<string, string>;
  // 422 body shape — error message plus per-section probe results.
  error?: string;
}

const ADMIN_TOKEN_KEY = "forage.adminToken";

export function adminToken(): string {
  if (typeof window === "undefined") return "";
  return localStorage.getItem(ADMIN_TOKEN_KEY) || "";
}

export function setAdminToken(token: string) {
  const trimmed = token.trim();
  if (trimmed === "") {
    localStorage.removeItem(ADMIN_TOKEN_KEY);
  } else {
    localStorage.setItem(ADMIN_TOKEN_KEY, trimmed);
  }
}

function authHeaders(): Record<string, string> {
  const token = adminToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

export async function fetchConfig(): Promise<ConfigFieldsResponse> {
  const r = await fetch(foragerBase() + "/config", {
    headers: authHeaders(),
    credentials: "include",
  });
  if (!r.ok) return throwForStatus(r);
  return r.json();
}

export async function saveConfig(
  patch: ConfigPatch,
  opts: { force?: boolean } = {},
): Promise<SaveConfigResponse> {
  const qs = opts.force ? "?force=true" : "";
  const r = await fetch(foragerBase() + "/config" + qs, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    credentials: "include",
    body: JSON.stringify(patch),
  });
  const body = await r.json().catch(() => ({}));
  // 422 is a normal "probes failed" response the caller renders inline; any
  // other non-2xx (incl. a 401 once auth is on) is a hard error.
  if (!r.ok && r.status !== 422) {
    fireUnauthorized(r.status);
    throw new ApiError(r.status, body.error || `HTTP ${r.status}`);
  }
  return { ok: r.ok, ...body } as SaveConfigResponse;
}

export async function testSection(
  section: string,
  patch: ConfigPatch,
): Promise<ProbeResult> {
  const r = await fetch(foragerBase() + `/config/test/${section}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    credentials: "include",
    body: JSON.stringify(patch),
  });
  if (!r.ok) return throwForStatus(r);
  const body = await r.json();
  return body.result as ProbeResult;
}

// stashDBFromStash reads the user's StashDB connection (endpoint + api key)
// straight from their Stash, so the setup wizard can pre-fill the StashDB
// step instead of making them paste the key again. Pass the Stash URL +
// key being set up (before they're saved). Returns found:false when Stash
// has no stashdb.org box configured.
export async function stashDBFromStash(
  stashUrl: string,
  stashApiKey: string,
): Promise<{ found: boolean; url?: string; api_key?: string }> {
  const r = await fetch(foragerBase() + "/config/stashdb-from-stash", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    credentials: "include",
    body: JSON.stringify({ stashUrl, stashApiKey }),
  });
  if (!r.ok) return { found: false };
  return r.json();
}

// refreshPerformers forces an immediate re-sync of the performer cache from
// Stash (the fast pull, no studio sweep) — so a just-created performer shows
// up in search right away instead of waiting for the 6h cache tick.
export function refreshPerformers(): Promise<{ ok: boolean }> {
  return postJSON("/refresh/performers", {});
}

export function refreshStudios(): Promise<{ ok: boolean }> {
  return postJSON("/refresh/studios", {});
}

// ── Download-client setup check (read-only diagnostic) ─────────────────

export interface ClientCheck {
  client: string; // "qbit" | "sab"
  category: string;
  category_exists: boolean;
  save_path?: string;
  hardlink_checked: boolean;
  hardlink_ok: boolean;
  detail: string;
  status: "ok" | "warn" | "error";
}

export interface DownloadSetup {
  library_root: string;
  checks: ClientCheck[];
}

// fetchDownloadSetup inspects the configured download clients: does forage's
// category exist, where does it save, and will placement hardlink into the
// library? Read-only — it never changes the client, just reports what to fix.
export function fetchDownloadSetup(): Promise<DownloadSetup> {
  return get<DownloadSetup>("/download-setup");
}

// ── Watches (track a scene → notified when a release at the target
//    quality appears; the server never grabs, you do) ─────────────────

export type WatchTarget = "any" | "480p" | "720p" | "1080p" | "4k";
// "grabbed" is terminal: the user grabbed the release; the watch lingers
// (greyed) so its batch's progress reads correctly until cleared.
export type WatchStatus = "watching" | "available" | "grabbed";

export interface Watch {
  stashdb_id: string;
  title: string;
  date?: string;
  studio_name?: string;
  image_url?: string;
  performer_name?: string;
  performer_id?: string;
  // Vestigial: the matcher no longer filters on a quality target (it surfaces
  // the best release by your preference ranking). Server still returns it.
  target?: WatchTarget;
  status: WatchStatus;
  found_title?: string;
  found_url?: string;
  found_indexer?: string;
  found_protocol?: string;
  found_size?: number;
  created_at: number;
  last_checked: number;
  found_at?: number;
  // Batch grouping: watches launched together (a performer's "watch all
  // missing", or a Discover multi-select) share batch_id; the Watching tab
  // groups by it. Empty = an ungrouped single track.
  batch_id?: string;
  batch_label?: string;
  // The verified candidate list captured when the watch went available, so
  // the user can re-pick a different release than the auto-chosen best.
  //
  // Omitted by the list endpoint for GRABBED watches, which is where the
  // weight was: 13.9 MB of 14.9 MB across 1528 finished rows that render a
  // single row and can never expand. Available and watching watches, the
  // ones that actually use the list, still carry theirs.
  candidates?: SceneRelease[];
  // The true number of stored candidates, present even where candidates
  // itself was omitted. Decides whether a card can expand.
  candidate_count?: number;
  // When the watch was grabbed (status="grabbed").
  grabbed_at?: number;
  // How many times this scene has been re-searched for a release (loop claims +
  // manual search-now). Shown as a badge so you can see how hard forage tried.
  search_count?: number;
  // Transient: a manual "search now" is actively re-searching this watch.
  searching?: boolean;
}

// AddWatchReq is one scene to watch — shared by the single add and the batch.
export interface AddWatchReq {
  stashdb_id: string;
  title: string;
  date?: string;
  studio?: string;
  image_url?: string;
  performer_name?: string;
  performer_id?: string;
  // All of the scene's performer names — stored on the watch so a re-search
  // needs no StashDB fetch and covers every performer (releases are often
  // named under a non-tracked one).
  performers?: string[];
  // Optional/vestigial — the matcher ignores it (best release by preference
  // ranking; quality floors live in release reject rules). Omit it.
  target?: WatchTarget;
  // Group membership for a SINGLE add: subject pages pass their stable
  // per-subject batch so one-by-one watches land in the same Watching-tab
  // group a multi-select would create. Omit for an ungrouped track.
  batch_id?: string;
  batch_label?: string;
}

export function addWatch(req: AddWatchReq): Promise<{ ok: boolean }> {
  return postJSON("/watches", req);
}

// addWatches creates many watches in one request under a shared batch, so the
// Watching tab groups them and shows their collective progress. Used by
// "watch all missing for a performer" and Discover multi-select. batch_id
// pins the group id (subject pages pass "<kind>:<subject id>" so repeat
// selections merge); omitted, the server generates a fresh one.
export function addWatches(req: {
  batch_id?: string;
  batch_label?: string;
  watches: AddWatchReq[];
}): Promise<{ ok: boolean; batch_id: string; count: number }> {
  return postJSON("/watches/batch", req);
}

export function fetchWatches(signal?: AbortSignal): Promise<{ watches: Watch[] }> {
  return get<{ watches: Watch[] }>("/watches", signal);
}

// Actionable, current-attention counts for the header bell + Watching-tab
// badge. (No "new scenes" — discovery isn't an alert.)
export interface NotificationCounts {
  watches_available: number;
  grabs_stalled: number;
  grabs_place_failing: number;
  grabs_failed: number;
}

export function fetchNotifications(
  signal?: AbortSignal,
): Promise<NotificationCounts> {
  return get<NotificationCounts>("/notifications", signal);
}

export async function deleteWatch(stashDBID: string): Promise<void> {
  const r = await fetch(foragerBase() + `/watches/${encodeURIComponent(stashDBID)}`, {
    method: "DELETE",
    headers: authHeaders(),
    credentials: "include",
  });
  if (!r.ok) await throwForStatus(r);
}

export function grabWatch(stashDBID: string): Promise<{ ok: boolean }> {
  return postJSON(`/watches/${encodeURIComponent(stashDBID)}/grab`, {});
}

// grabWatchCandidate grabs a SPECIFIC release from the watch's stored
// candidate list (a re-pick when the auto-chosen best isn't what you want).
export function grabWatchCandidate(
  stashDBID: string,
  downloadUrl: string,
): Promise<{ ok: boolean }> {
  return postJSON(`/watches/${encodeURIComponent(stashDBID)}/grab-candidate`, {
    download_url: downloadUrl,
  });
}

// searchWatches kicks an immediate, server-bounded re-search of still-watching
// scenes, bypassing the 30-min loop so they surface releases now. Scope by an
// explicit id set (a group's exact rows) or a batch, or nothing for all
// watching. Returns how many it's searching; the per-watch `searching` flag +
// list poll show progress. 409 if one's already running.
export function searchWatches(opts?: {
  ids?: string[];
  batchId?: string;
}): Promise<{ ok: boolean; searching: number }> {
  const body: { ids?: string[]; batch_id?: string } = {};
  if (opts?.ids?.length) body.ids = opts.ids;
  else if (opts?.batchId) body.batch_id = opts.batchId;
  return postJSON("/watches/search-now", body);
}

// clearWatchBatch removes every watch in a batch (the per-batch "Clear",
// typically once a collection batch is fully grabbed).
export async function clearWatchBatch(batchId: string): Promise<void> {
  const r = await fetch(
    foragerBase() + `/watches/batch/${encodeURIComponent(batchId)}`,
    { method: "DELETE", headers: authHeaders(), credentials: "include" },
  );
  if (!r.ok) await throwForStatus(r);
}

// dismissWatch rejects the watch's current found release (e.g. it's dead or
// over-compressed): ignores that exact release going forward and flips the
// watch back to watching so the loop surfaces a different one.
export function dismissWatch(stashDBID: string): Promise<{ ok: boolean }> {
  return postJSON(`/watches/${encodeURIComponent(stashDBID)}/dismiss`, {});
}

// redoWatch discards a grabbed (or found) release the user decided was bad —
// it purges the grab (download + any placed file/scene) so it can't re-flip
// the watch, ignores that release going forward, and flips the watch back to
// watching so it can be re-searched for a different one.
export function redoWatch(
  stashDBID: string,
): Promise<{ ok: boolean; purged?: string[] }> {
  return postJSON(`/watches/${encodeURIComponent(stashDBID)}/redo`, {});
}
// ── Destruction journal ─────────────────────────────────────────────

// DestructionEntry is one journal row: something forage destroyed, trashed,
// refused or restored, with the complete file list snapshotted at decision
// time. outcome "trashed" entries are restorable until the TTL sweep runs.
export interface DestructionEntry {
  id: number;
  reason: string;
  scene_id?: string;
  title?: string;
  files: { path: string; size?: number }[];
  outcome: "intent" | "destroyed" | "trashed" | "refused" | "failed" | "restored";
  detail?: string;
  created_at: number;
}

export function fetchDestructions(limit = 100): Promise<DestructionEntry[]> {
  return get<{ destructions: DestructionEntry[] }>(
    `/destructions?limit=${limit}`,
  ).then((r) => r.destructions);
}

export async function restoreDestruction(id: number): Promise<void> {
  const r = await fetch(foragerBase() + `/destructions/${id}/restore`, {
    method: "POST",
    headers: authHeaders(),
  });
  if (!r.ok) {
    const body = await r.json().catch(() => ({}) as { error?: string });
    throw new Error(
      (body as { error?: string }).error || `restore failed (${r.status})`,
    );
  }
}

