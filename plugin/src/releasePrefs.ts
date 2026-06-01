// Friendly (no-typing) release-ranking preferences and the COMPILER that
// turns them into the daemon's existing scoring.Rule[] list. The daemon
// never sees this structure — it keeps scoring off releaseRules. Friendly
// mode is purely a client-side compiler: the user expresses preference by
// ordering + simple toggles, and we generate the regex/points rules.
//
// The point spacing is the load-bearing part. It must guarantee the
// documented layering: RESOLUTION dominates; indexer + protocol are
// tie-breakers WITHIN a resolution tier and can never lift a release past
// the next tier up. See assertSpacingInvariants() for the proof-by-check.

import type { ReleaseRule } from "./api";

// The four fixed resolution tiers, most-to-least specific. Friendly mode
// only ever reorders these — it never invents new ones.
export type Resolution = "4k" | "1080p" | "720p" | "480p";

export const ALL_RESOLUTIONS: Resolution[] = ["4k", "1080p", "720p", "480p"];

// resolutionFloor: "" = grab anything; otherwise reject every resolution
// strictly BELOW the named floor (so "720p" rejects 480p, "1080p" rejects
// 480p + 720p; 4k is above all and never rejected).
export type ResolutionFloor = "" | "480p" | "720p" | "1080p";

export type ProtocolPref = "torrent" | "none" | "usenet";

export interface ReleasePrefs {
  // Resolution tiers in PREFERENCE order (index 0 = most preferred).
  resolutionOrder: Resolution[];
  // Reject anything strictly below this resolution. "" = no floor.
  resolutionFloor: ResolutionFloor;
  // Indexer NAMES in preference order (index 0 = most preferred). Names not
  // present in the user's Prowlarr are simply ignored at compile time.
  indexerOrder: string[];
  // Indexer NAMES to hard-exclude (reject). Disjoint from indexerOrder in
  // the UI, but compile is robust either way.
  blockedIndexers: string[];
  // Protocol tie-breaker preference.
  protocolPref: ProtocolPref;
}

// defaultPrefs mirrors the daemon's current DefaultRules ordering:
// 1080p > 4k > 720p > 480p, prefer usenet, no indexer prefs, no floor.
export function defaultPrefs(): ReleasePrefs {
  return {
    resolutionOrder: ["1080p", "4k", "720p", "480p"],
    resolutionFloor: "",
    indexerOrder: [],
    blockedIndexers: [],
    protocolPref: "usenet",
  };
}

// parsePrefs reads a stored prefs JSON string, reconciling it against the
// fixed resolution set so a partial/older blob still yields all four tiers
// in a sensible order. Falls back to defaults on empty/garbage. Pure — used
// by both the friendly editor and the "back to simple" recompile path.
export function parsePrefs(json: string): ReleasePrefs {
  const d = defaultPrefs();
  if (!json.trim()) return d;
  try {
    const p = JSON.parse(json) as Partial<ReleasePrefs>;
    const order = (p.resolutionOrder ?? []).filter((r): r is Resolution =>
      ALL_RESOLUTIONS.includes(r as Resolution),
    );
    for (const r of ALL_RESOLUTIONS) if (!order.includes(r)) order.push(r);
    return {
      resolutionOrder: order,
      resolutionFloor: (p.resolutionFloor ?? "") as ResolutionFloor,
      indexerOrder: Array.isArray(p.indexerOrder) ? p.indexerOrder : [],
      blockedIndexers: Array.isArray(p.blockedIndexers) ? p.blockedIndexers : [],
      protocolPref: (p.protocolPref ?? "usenet") as ProtocolPref,
    };
  } catch {
    return d;
  }
}

// ── Point scheme (keep these in sync with assertSpacingInvariants) ────────

// Resolution tiers are 40 apart, centred so the top tier is +100:
// rank 0 → 100, 1 → 60, 2 → 20, 3 → -20.
const RES_TOP = 100;
const RES_GAP = 40;

// Indexer bonus is a small descending spread, top = +12 down to 0, so it
// tie-breaks within a tier without ever crossing one.
const INDEXER_TOP = 12;

// Protocol preference is +25 (matches the shipped "prefer usenet" default).
const PROTOCOL_BONUS = 25;

// The max swing a single release can gain from indexer + protocol combined.
// MUST stay strictly below RES_GAP for resolution to dominate.
const MAX_TIEBREAK_SWING = INDEXER_TOP + PROTOCOL_BONUS; // 37 < 40 ✓

// The known resolution title patterns — copied verbatim from
// scoring.DefaultRules() on the server so friendly-compiled rules match
// exactly what hand-written ones would.
const RES_PATTERN: Record<Resolution, string> = {
  "4k": "\\b(2160p?|3840p?|4k|uhd)\\b",
  "1080p": "\\b1080p?\\b",
  "720p": "\\b720p?\\b",
  "480p": "\\b(480p?|360p?|\\bsd\\b)\\b",
};

const RES_LABEL: Record<Resolution, string> = {
  "4k": "4K / 2160p",
  "1080p": "1080p",
  "720p": "720p",
  "480p": "480p / SD",
};

// resPoints maps a 0-based rank into the 40-spaced, +100-top scheme.
function resPoints(rank: number): number {
  return RES_TOP - rank * RES_GAP;
}

// indexerPoints spreads INDEXER_TOP..0 linearly across n ranked indexers
// (top = INDEXER_TOP, bottom = 0). A single indexer gets the top bonus.
function indexerPoints(rank: number, n: number): number {
  if (n <= 1) return INDEXER_TOP;
  return Math.round((INDEXER_TOP * (n - 1 - rank)) / (n - 1));
}

// escapeRegex escapes regex metacharacters so an indexer name compiles as a
// literal pattern. Most names ("1337x", "PornoLab") are literal-safe, but
// names with dots/parens/+ would otherwise misbehave.
function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// floorRejectsBelow returns the resolutions strictly below a floor, in the
// fixed worst-to-best order (so "1080p" → ["480p","720p"]). 4k is never
// below any floor.
function floorRejectsBelow(floor: ResolutionFloor): Resolution[] {
  if (floor === "") return [];
  // worst → best ranking used for "strictly below" comparison
  const order: Resolution[] = ["480p", "720p", "1080p", "4k"];
  const floorIdx = order.indexOf(floor);
  return order.slice(0, floorIdx);
}

// compileRules turns friendly prefs into the daemon's flat scoring.Rule[].
// Pure + deterministic — the single place friendly mode generates rules.
// Order of emitted rules is cosmetic (scoring is additive), but we group
// them resolution → floor rejects → indexer bonuses → indexer blocks →
// protocol so the advanced editor shows a readable list.
export function compileRules(prefs: ReleasePrefs): ReleaseRule[] {
  const rules: ReleaseRule[] = [];

  // Resolution tiers (descending points, 40 apart).
  prefs.resolutionOrder.forEach((res, rank) => {
    rules.push({
      label: RES_LABEL[res],
      on: "title",
      pattern: RES_PATTERN[res],
      points: resPoints(rank),
    });
  });

  // Floor: reject everything strictly below it.
  floorRejectsBelow(prefs.resolutionFloor).forEach((res) => {
    rules.push({
      label: `below ${prefs.resolutionFloor} — skip`,
      on: "title",
      pattern: RES_PATTERN[res],
      points: 0,
      reject: true,
    });
  });

  // Indexer bonuses (skip blocked names + zero-point tail to avoid clutter).
  const blocked = new Set(prefs.blockedIndexers);
  const ranked = prefs.indexerOrder.filter((n) => !blocked.has(n));
  ranked.forEach((name, rank) => {
    const points = indexerPoints(rank, ranked.length);
    if (points <= 0) return; // a 0-point rule does nothing
    rules.push({
      label: name,
      on: "indexer",
      pattern: escapeRegex(name),
      points,
    });
  });

  // Blocked indexers → hard reject.
  prefs.blockedIndexers.forEach((name) => {
    rules.push({
      label: `block ${name}`,
      on: "indexer",
      pattern: escapeRegex(name),
      points: 0,
      reject: true,
    });
  });

  // Protocol tie-breaker.
  if (prefs.protocolPref === "usenet" || prefs.protocolPref === "torrent") {
    rules.push({
      label: prefs.protocolPref === "usenet" ? "prefer usenet" : "prefer torrents",
      on: "protocol",
      pattern: prefs.protocolPref,
      points: PROTOCOL_BONUS,
    });
  }

  return rules;
}

// assertSpacingInvariants throws if the compiled point scheme could ever let
// a lower resolution tier (plus its best indexer + protocol bonus) beat a
// higher tier (with its worst-case zero bonus). This is the documented
// guarantee; the check is cheap enough to run wherever it's useful (tests,
// a dev-time sanity call). Returns the prefs so it can wrap a value inline.
export function assertSpacingInvariants(): void {
  // The only thing that can vary within a tier is the tie-break swing.
  // Resolution tiers are RES_GAP apart, so the guarantee reduces to:
  if (MAX_TIEBREAK_SWING >= RES_GAP) {
    throw new Error(
      `release-prefs spacing broken: tie-break swing ${MAX_TIEBREAK_SWING} >= resolution gap ${RES_GAP}`,
    );
  }
  // Spot-check the concrete numbers for a 4-tier order with a best-case
  // lower tier vs a worst-case upper tier, for every adjacent pair.
  for (let rank = 0; rank < ALL_RESOLUTIONS.length - 1; rank++) {
    const upperWorst = resPoints(rank); // upper tier, no bonuses
    const lowerBest = resPoints(rank + 1) + MAX_TIEBREAK_SWING;
    if (lowerBest >= upperWorst) {
      throw new Error(
        `release-prefs spacing broken at rank ${rank}: lower-best ${lowerBest} >= upper-worst ${upperWorst}`,
      );
    }
  }
}
