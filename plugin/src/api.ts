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
}

export function fetchDiscover(opts?: {
  days?: number;
  favoriteOnly?: boolean;
  limit?: number;
  trendingLimit?: number;
}): Promise<DiscoverResponse> {
  const params = new URLSearchParams();
  if (opts?.days != null) params.set("days", String(opts.days));
  if (opts?.favoriteOnly) params.set("favorite_only", "true");
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

export interface MissingResponse {
  performer: {
    local_id: string;
    stashdb_id: string;
    name: string;
  };
  total_scenes: number;
  owned_count: number;
  missing: MissingScene[];
}

export function fetchMissing(localPerformerID: string): Promise<MissingResponse> {
  return get<MissingResponse>(`/missing-scenes?performer=${encodeURIComponent(localPerformerID)}`);
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
  | "downloading"
  | "completed"
  | "placed"
  | "scanned"
  | "confirmed"
  | "mismatched"
  | "orphaned"
  | "failed";

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
}

export interface GrabsResponse {
  grabs: Grab[];
  totals: Partial<Record<GrabStatus, number>>;
}

export function fetchGrabs(opts?: {
  status?: GrabStatus | "any";
  limit?: number;
  offset?: number;
}): Promise<GrabsResponse> {
  const params = new URLSearchParams();
  if (opts?.status && opts.status !== "any") params.set("status", opts.status);
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
  // Ranked local-performer guesses from the release title — the one-click
  // options for reassigning a mis-filed / Unsorted grab. Absent for packs.
  performer_suggestions?: {
    stash_id: string;
    name: string;
    scene_count: number;
    favorite: boolean;
  }[];
}

export function fetchGrabDetail(id: number): Promise<GrabDetail> {
  return get<GrabDetail>(`/grabs/${id}/detail`);
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
];

export function isActiveStatus(s: GrabStatus): boolean {
  return ACTIVE_STATUSES.includes(s);
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
  sabConfigured: boolean;
  sabCategory: string;
  placerConfigured: boolean;
  libraryRoot: string;
  unconfigured: boolean;
  // True when a credential is configured (password OR API key) and the UI
  // must show a login gate.
  adminAuthRequired: boolean;
  // True when a username+password is set — the UI shows the password login
  // form; false (with adminAuthRequired) means a token-only daemon, so the
  // UI falls back to the API-key field.
  passwordSet: boolean;
}

export function fetchHealth(): Promise<Health> {
  return get<Health>("/healthz");
}

// IndexerInfo is one configured Prowlarr indexer, as the friendly
// release-prefs editor needs it: a name to rank/block, a protocol badge,
// and whether it's enabled.
export interface IndexerInfo {
  name: string;
  protocol: string; // "torrent" | "usenet"
  enabled: boolean;
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
}

export interface ProbeResult {
  ok: boolean;
  message?: string;
}

export interface SaveConfigResponse {
  ok: boolean;
  results: Record<string, ProbeResult>;
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

// ── Collection jobs (server-side multi-scene grabs) ────────────────────

export type JobSceneStatus =
  | "pending"
  | "found"
  | "grabbed"
  | "no_match"
  | "no_result"
  | "error"
  | "skipped";

export interface JobScene {
  stashdb_id: string;
  title: string;
  status: JobSceneStatus;
  release?: string;
  // Present only in the job-DETAIL response (GET /jobs/{id}): the full
  // verified release list + which is picked, so the job re-opens as the
  // interactive collection view.
  candidates?: SceneRelease[];
  picked_url?: string;
}

export interface CollectionJob {
  id: string;
  performer_id: string;
  performer_name: string;
  state: "running" | "done" | "cancelled";
  total: number;
  done: number;
  found: number;
  grabbed: number;
  started_at: number;
  finished_at?: number;
  scenes: JobScene[];
}

// startCollectionJob kicks off a server-side crawl. sceneIds empty/omitted
// = every missing scene for the performer; otherwise just that subset.
export function startCollectionJob(
  performerId: string,
  sceneIds?: string[],
): Promise<CollectionJob> {
  return postJSON<CollectionJob>("/jobs", {
    performer_id: performerId,
    scene_ids: sceneIds && sceneIds.length > 0 ? sceneIds : undefined,
  });
}

export function fetchCollectionJobs(
  signal?: AbortSignal,
): Promise<{ jobs: CollectionJob[] }> {
  return get<{ jobs: CollectionJob[] }>("/jobs", signal);
}

export async function cancelCollectionJob(id: string): Promise<void> {
  const r = await fetch(foragerBase() + `/jobs/${encodeURIComponent(id)}`, {
    method: "DELETE",
    headers: authHeaders(),
    credentials: "include",
  });
  if (!r.ok) await throwForStatus(r);
}

// fetchCollectionJob returns one job WITH per-scene candidate lists, for
// re-opening it as the interactive collection view.
export function fetchCollectionJob(id: string): Promise<CollectionJob> {
  return get<CollectionJob>(`/jobs/${encodeURIComponent(id)}`);
}

// grabJobScene grabs a specific stored candidate for one scene of a job
// (the re-pick path for scenes the auto-pass skipped).
export function grabJobScene(
  jobId: string,
  sceneId: string,
  downloadUrl: string,
): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>(`/jobs/${encodeURIComponent(jobId)}/grab`, {
    scene_id: sceneId,
    download_url: downloadUrl,
  });
}

// ── Watches (track a scene → notified when a release at the target
//    quality appears; the server never grabs, you do) ─────────────────

export type WatchTarget = "any" | "480p" | "720p" | "1080p" | "4k";
export type WatchStatus = "watching" | "available";

export interface Watch {
  stashdb_id: string;
  title: string;
  date?: string;
  studio_name?: string;
  image_url?: string;
  performer_name?: string;
  performer_id?: string;
  target: WatchTarget;
  status: WatchStatus;
  found_title?: string;
  found_url?: string;
  found_indexer?: string;
  found_protocol?: string;
  found_size?: number;
  created_at: number;
  last_checked: number;
  found_at?: number;
}

export function addWatch(req: {
  stashdb_id: string;
  title: string;
  date?: string;
  studio?: string;
  image_url?: string;
  performer_name?: string;
  performer_id?: string;
  target: WatchTarget;
}): Promise<{ ok: boolean; target: WatchTarget }> {
  return postJSON("/watches", req);
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
