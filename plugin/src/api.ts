// Forager API client. All endpoints live at a single base URL — set
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
// over HTTPS and the configured forager URL is HTTP, the browser
// will block every fetch silently — no amount of CORS config helps.
export function mixedContentBlocked(): boolean {
  return (
    typeof location !== "undefined" &&
    location.protocol === "https:" &&
    foragerBase().startsWith("http:")
  );
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(foragerBase() + path, { signal });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error || `HTTP ${r.status}`);
  }
  return r.json() as Promise<T>;
}

async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const r = await fetch(foragerBase() + path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error || `HTTP ${r.status}`);
  }
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

// ── Stash UI integration ───────────────────────────────────────────
//
// The plugin loads from Stash's origin in production
// (e.g. `https://your-stash.example.com/plugin/forage/assets/index.html`).
// That lets us address Stash's performer images at
// `<origin>/performer/<id>/image` without any GraphQL call. In dev mode
// (`localhost:5173`) there's no Stash to talk to — performer images
// fall through to a placeholder.

export function stashBase(): string | null {
  if (typeof location === "undefined") return null;
  // Heuristic: localhost / 127.0.0.1 is the Vite dev server, not Stash.
  if (location.hostname === "localhost" || location.hostname === "127.0.0.1") {
    return null;
  }
  return location.origin;
}

export function performerImageURL(localStashID: string): string | null {
  const base = stashBase();
  if (!base) return null;
  return `${base}/performer/${encodeURIComponent(localStashID)}/image`;
}

export function fetchSceneReleases(
  stashDBID: string,
  signal?: AbortSignal,
): Promise<SceneReleasesResponse> {
  return get<SceneReleasesResponse>(
    `/scenes/${encodeURIComponent(stashDBID)}/releases`,
    signal,
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
    body: fd,
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error || `HTTP ${r.status}`);
  }
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
    body: fd,
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error || `HTTP ${r.status}`);
  }
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
  release_title: string;
  release_size?: number;
  release_indexer?: string;
  download_url?: string;
  client?: string;
  client_id?: string;
  client_name?: string;
  category?: string;
  status: GrabStatus;
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
  performers: { name: string; as?: string }[];
  local_scene_id?: string;
  stash_scene_url?: string;
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

export interface DeleteGrabResult {
  ok: boolean;
  removed: string[];
  errors?: string[];
}

export async function deleteGrab(id: number): Promise<DeleteGrabResult> {
  const r = await fetch(foragerBase() + `/grabs/${id}`, { method: "DELETE" });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error || `HTTP ${r.status}`);
  }
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
  adminAuthRequired: boolean;
}

export function fetchHealth(): Promise<Health> {
  return get<Health>("/healthz");
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
  pollInterval?: string;
  orphanAfter?: string;
  cacheRefresh?: string;
  allowedOrigin?: string;
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
  const r = await fetch(foragerBase() + "/config", { headers: authHeaders() });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error || `HTTP ${r.status}`);
  }
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
    body: JSON.stringify(patch),
  });
  const body = await r.json().catch(() => ({}));
  if (!r.ok && r.status !== 422) {
    throw new Error(body.error || `HTTP ${r.status}`);
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
    body: JSON.stringify(patch),
  });
  if (!r.ok) {
    const e = await r.json().catch(() => ({ error: r.statusText }));
    throw new Error(e.error || `HTTP ${r.status}`);
  }
  const body = await r.json();
  return body.result as ProbeResult;
}
