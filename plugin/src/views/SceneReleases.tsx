import { ArrowDownIcon, BlockedIcon, CheckIcon, CloseIcon, DeadIcon, ExternalIcon, RefreshIcon, StarIcon, WarnIcon } from "../icons";
import { useEffect, useState } from "react";
import {
  fetchSceneReleases,
  postGrab,
  type MatchExplain,
  type MissingPerformer,
  type SceneRelease,
  type SceneReleasesResponse,
} from "../api";
import { ResBadge } from "../ResBadge";
import WatchControl from "../WatchControl";
import { humanSize } from "../format";

// Map the matcher's 0..1 confidence into one of five outline tiers.
// Only applied to verified releases — the unverified section already
// stands apart visually.
function confTier(c: number): string {
  if (c >= 0.70) return "conf-1";
  if (c >= 0.55) return "conf-2";
  if (c >= 0.40) return "conf-3";
  if (c >= 0.25) return "conf-4";
  return "conf-5";
}

// confColor mirrors the confTier bands as concrete colours for the match
// meter (number + micro-bar). Kept in sync with the .release.conf-N CSS.
function confColor(c: number): string {
  if (c >= 0.7) return "#22c55e"; // accent-bright
  if (c >= 0.55) return "#16a34a"; // accent
  if (c >= 0.4) return "#84cc16"; // lime
  if (c >= 0.25) return "#d4a017"; // amber
  return "#b85454"; // red-clay
}

// Release sort is user-selectable because "best" is genuinely
// ambiguous among verified releases (all the same scene):
//   - quality:    highest resolution wins — usually what you want
//   - match:      matcher confidence — how strongly the filename's
//                 metadata matches the scene (NOT video quality)
//   - popularity: seeders (torrent) / grabs (usenet) — availability
const SORT_OPTIONS = [
  { value: "match", label: "Match" },
  { value: "quality", label: "Quality" },
  { value: "popularity", label: "Popularity" },
] as const;
type ReleaseSort = (typeof SORT_OPTIONS)[number]["value"];
// v2: default flipped from quality → match (the strongest match for the
// scene you're viewing should lead). Bumped key so the old default is
// dropped rather than pinning existing users to quality.
const SORT_STORAGE_KEY = "forage.releases.sort.v2";

function loadSort(): ReleaseSort {
  const s = localStorage.getItem(SORT_STORAGE_KEY) as ReleaseSort | null;
  if (s && SORT_OPTIONS.some((o) => o.value === s)) return s;
  return "match";
}

// resolutionRank extracts a sortable height from the release title.
// Falls back to 0 when no resolution token is present so unlabelled
// releases sink below labelled ones.
function resolutionRank(title: string): number {
  const t = title.toLowerCase();
  // 4K named by height (2160p) or width (3840p).
  if (/\b(2160p?|3840p?|4k|uhd)\b/.test(t)) return 2160;
  // FHD / FHDC are JAV/sukebei labels for Full HD = 1080p.
  if (/\b(1080p?|fhdc?)\b/.test(t)) return 1080;
  if (/\b720p?\b/.test(t)) return 720;
  if (/\b480p?\b/.test(t)) return 480;
  return 0;
}

// seedTier buckets torrent seed health (mirrors the backend) so a marginal
// file-size edge can't promote a barely-seeded release over a well-seeded one
// of equal score. Usenet has no seeders → treated as healthy.
function seedTier(r: SceneRelease): number {
  if (r.protocol !== "torrent") return 3;
  const s = r.seeders ?? 0;
  if (s >= 20) return 3;
  if (s >= 5) return 2;
  if (s >= 1) return 1;
  return 0;
}

// releaseKey is a stable per-release identity for React keys + grab
// state. download_url alone isn't safe: magnet-only indexers (TPB) can
// share an empty download_url, which would collapse unrelated rows into
// one state bucket (clicking one appears to grab/fail them all).
function releaseKey(r: SceneRelease): string {
  return r.download_url || r.info_url || r.title;
}

// grabbable: a torrent with zero seeders can't actually be downloaded.
// Usenet has no seeder notion, so it's always grabbable.
function grabbable(r: SceneRelease): boolean {
  return !(r.protocol === "torrent" && r.seeders === 0);
}

// healthClass tints the seeder/grab count: dead (0 seeders) red, low
// (≤3, or 0 usenet grabs) amber, otherwise neutral.
function healthClass(r: SceneRelease): string {
  if (r.protocol === "usenet") return r.grabs === 0 ? "low" : "";
  if (r.seeders === 0) return "dead";
  if (r.seeders <= 3) return "low";
  return "";
}

function sortReleases(rels: SceneRelease[], sort: ReleaseSort): SceneRelease[] {
  const out = [...rels];
  out.sort((a, b) => {
    // Dead torrents sink in the quality-oriented modes (popularity mode
    // already orders by availability, so leave it pure).
    if (sort !== "popularity") {
      const ga = grabbable(a);
      const gb = grabbable(b);
      if (ga !== gb) return ga ? -1 : 1;
    }
    if (sort === "quality") {
      // Lead with the preference SCORE (resolution + indexer + protocol
      // rules), so this reflects the user's full ranking — not resolution
      // alone. Fall back to resolution for releases the scorer didn't touch.
      const sd = (b.score ?? 0) - (a.score ?? 0);
      if (sd !== 0) return sd;
      const d = resolutionRank(b.title) - resolutionRank(a.title);
      if (d !== 0) return d;
      const ht = seedTier(b) - seedTier(a); // healthier seeds before size
      if (ht !== 0) return ht;
      if (b.size !== a.size) return b.size - a.size; // bigger encode wins
      return b.popularity - a.popularity;
    }
    if (sort === "match") {
      if (b.confidence !== a.confidence) return b.confidence - a.confidence;
      // tie on match → prefer the user's score, then bitrate, then availability
      const sd = (b.score ?? 0) - (a.score ?? 0);
      if (sd !== 0) return sd;
      const d = resolutionRank(b.title) - resolutionRank(a.title);
      if (d !== 0) return d;
      const ht = seedTier(b) - seedTier(a);
      if (ht !== 0) return ht;
      if (b.size !== a.size) return b.size - a.size;
      return b.popularity - a.popularity;
    }
    // popularity
    if (b.popularity !== a.popularity) return b.popularity - a.popularity;
    return b.confidence - a.confidence;
  });
  return out;
}

type GrabState =
  | { status: "idle" }
  | { status: "grabbing" }
  | { status: "queued"; client: string }
  | { status: "error"; message: string };

export default function SceneReleases({
  sceneId,
  performerName,
}: {
  sceneId: string;
  // Set by the parent route — whichever performer page the user
  // navigated from. Threaded into /grab so forage places the file
  // under <library_root>/<performer>/.
  performerName?: string;
}) {
  const [data, setData] = useState<SceneReleasesResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Per-release grab state, keyed by releaseKey (download_url is empty/
  // shared for some magnet-only indexers, which would group unrelated rows).
  const [grabs, setGrabs] = useState<Record<string, GrabState>>({});
  const [showUnverified, setShowUnverified] = useState(false);
  const [sort, setSort] = useState<ReleaseSort>(loadSort);
  // Manual alias override: when the default (library name + StashDB
  // spellings) finds nothing, the user can retry under a specific name a
  // tracker might have used. Null = use the automatic names.
  const [alias, setAlias] = useState<string | null>(null);
  // Deep search: the default search is LEAN (primary performer × studio +
  // title — ~2 Prowlarr queries, fast). Deep flips to the full fan-out
  // (every spelling × studio/year + bare performer). Far more thorough but
  // many more queries, so it's opt-in for when lean comes up short. The
  // component is keyed by sceneId in App, so this resets per scene.
  const [deep, setDeep] = useState(false);
  // Whether the alias-retry panel is expanded (its trigger lives in the
  // controls row, next to Deep search).
  const [aliasOpen, setAliasOpen] = useState(false);

  useEffect(() => {
    localStorage.setItem(SORT_STORAGE_KEY, sort);
  }, [sort]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setData(null);
    setError(null);
    fetchSceneReleases(sceneId, {
      performer: performerName,
      alias: alias || undefined,
      lean: !deep,
    })
      .then((r) => {
        if (cancelled) return;
        setData(r);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(e.message);
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [sceneId, performerName, alias, deep]);

  if (loading)
    return (
      <div className="empty">
        {deep
          ? "Deep searching all trackers… (this casts a wide net, give it a moment)"
          : "Searching for releases…"}
      </div>
    );
  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!data) return null;

  const verified = sortReleases(
    data.releases.filter((r) => r.verified),
    sort,
  );
  const unverified = sortReleases(
    data.releases.filter((r) => !r.verified),
    sort,
  );

  // The recommended grab: the QUALITY-ranked top among verified,
  // non-rejected, grabbable releases — computed with the same seed-health-
  // aware ranking the list uses, so the pick never disagrees with the order
  // and a marginally-larger file can't beat a far-better-seeded one. Fixed to
  // "quality" regardless of the user's current display sort, so the
  // recommendation is stable.
  const best = sortReleases(
    data.releases.filter((r) => r.verified && !r.rejected && grabbable(r)),
    "quality",
  )[0];
  const bestKey = best ? releaseKey(best) : null;

  // grab queues a release. sceneIdOverride lets the user grab an
  // unverified release AS the scene the matcher actually thinks it is
  // (the "Looks like X" pick) — so forage predicts/identifies against the
  // right StashDB scene, not the one being viewed. confOverride carries
  // that pick's confidence for the grab record.
  const grab = async (
    rel: SceneRelease,
    sceneIdOverride?: string,
    confOverride?: number,
    force?: boolean,
  ) => {
    const k = releaseKey(rel);
    setGrabs((g) => ({ ...g, [k]: { status: "grabbing" } }));
    try {
      const res = await postGrab({
        download_url: rel.download_url,
        release_title: rel.title,
        release_size: rel.size,
        release_indexer: rel.indexer,
        protocol: rel.protocol,
        scene_id: sceneIdOverride ?? data.scene.stashdb_id,
        confidence: confOverride ?? rel.confidence,
        performer_name: performerName,
        force,
      });
      setGrabs((g) => ({
        ...g,
        [k]: { status: "queued", client: res.client || "?" },
      }));
    } catch (e) {
      setGrabs((g) => ({
        ...g,
        [k]: {
          status: "error",
          message: (e as Error).message,
        },
      }));
    }
  };

  return (
    <div>
      <div className="scene-hero">
        {data.scene.image_url && (
          <img
            className="scene-hero-img"
            src={data.scene.image_url}
            alt=""
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.display = "none";
            }}
          />
        )}
        <div className="scene-hero-info">
          <h2>{data.scene.title || "(untitled)"}</h2>
          <div className="meta">
            {data.scene.date && <span>{data.scene.date}</span>}
            {data.scene.date && data.scene.studio && <span> · </span>}
            {data.scene.studio && <span>{data.scene.studio}</span>}
          </div>
          {data.scene.performers.length > 0 && (
            <div className="meta">
              {data.scene.performers.map((p) => p.name).join(", ")}
            </div>
          )}
          <div className="meta">
            <a
              href={`https://stashdb.org/scenes/${data.scene.stashdb_id}`}
              target="_blank"
              rel="noopener noreferrer"
            >
              View on StashDB <ExternalIcon size={11} />
            </a>
          </div>
          {/* Nothing here good enough? Watch the scene for a future
              release at a chosen quality instead of grabbing now. */}
          <WatchControl
            scene={{
              stashdb_id: data.scene.stashdb_id,
              title: data.scene.title,
              date: data.scene.date,
              studio: data.scene.studio,
              image_url: data.scene.image_url,
            }}
            performerName={performerName || data.scene.performers[0]?.name}
            variant="inline"
          />
        </div>
      </div>

      {/* Controls row — always present. Sort only when there are results
          to sort; the two search affordances (alias retry + deep) sit
          together on the right, same pill styling. */}
      <div className="release-controls">
        {(verified.length > 0 || unverified.length > 0) && (
          <select
            className="sort-select"
            value={sort}
            onChange={(e) => setSort(e.target.value as ReleaseSort)}
          >
            {SORT_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                Sort: {o.label}
              </option>
            ))}
          </select>
        )}
        <div className="release-controls-search">
          <button
            className={"deep-search-btn" + (aliasOpen || alias ? " on" : "")}
            onClick={() => setAliasOpen((v) => !v)}
            title="Trackers sometimes list a release under a different name spelling, retry under another name"
          >
            {alias ? `Name: ${alias}` : "Different name"}
          </button>
          {!deep && (
            <button
              className="deep-search-btn"
              onClick={() => setDeep(true)}
              title="Run the full multi-tracker fan-out, slower, but catches releases the quick search misses"
            >
              Deep search <RefreshIcon size={11} />
            </button>
          )}
        </div>
      </div>

      {aliasOpen && (
        <AliasRetry
          active={alias}
          performers={data.scene.performers}
          onSearch={(a) => {
            setAlias(a || null);
            // An explicit alias retry means the user is digging — go deep so
            // the alias is searched across every term, not just the lean pair.
            if (a) setDeep(true);
          }}
        />
      )}

      {verified.length === 0 && unverified.length === 0 ? (
        <div className="empty">
          No releases found{alias ? ` for "${alias}"` : " for this scene"}.
          {!deep ? (
            <div className="empty-hint">
              That was a quick search.{" "}
              <button className="deep-search-btn" onClick={() => setDeep(true)}>
                Deep search all trackers <RefreshIcon size={11} />
              </button>{" "}
              to cast a wider net, or try a different name above.
            </div>
          ) : (
            !alias && (
              <div className="empty-hint">
                Trackers sometimes list a different name spelling, try the
                “Different name” button above.
              </div>
            )
          )}
        </div>
      ) : (
        <>
          {verified.length > 0 && (
            <section>
              <h3 className="section-header">Verified ({verified.length})</h3>
              <ReleaseList
                releases={verified}
                grabs={grabs}
                onGrab={grab}
                bestKey={bestKey}
                gateLabels={data.gate_labels}
              />
            </section>
          )}
          {unverified.length > 0 && (
            <section>
              <h3
                className="section-header collapsible"
                onClick={() => setShowUnverified((v) => !v)}
              >
                <span
                  className={"fchev" + (showUnverified ? " open" : "")}
                  aria-hidden="true"
                />{" "}
                Unverified ({unverified.length}), different scenes that share a
                title token
              </h3>
              {showUnverified && (
                <ReleaseList
                  releases={unverified}
                  grabs={grabs}
                  onGrab={grab}
                  gateLabels={data.gate_labels}
                />
              )}
            </section>
          )}
        </>
      )}
    </div>
  );
}

// AliasRetry lets the user re-run the release search under a specific name
// spelling — for when a tracker indexed the release under an alias the
// automatic names (library name + StashDB spellings) didn't cover. Offers
// the scene's performer names AND each performer's Stash aliases as
// one-click chips, plus a small free-text pill for anything not listed.
function AliasRetry({
  active,
  performers,
  onSearch,
}: {
  active: string | null;
  performers: MissingPerformer[];
  onSearch: (alias: string) => void;
}) {
  const [text, setText] = useState("");
  const [typing, setTyping] = useState(false);

  // Build the suggestion list: each performer's name, then their aliases,
  // de-duplicated (case-insensitive) preserving order. Names lead because
  // they're the most likely spellings.
  const seen = new Set<string>();
  const suggestions: string[] = [];
  const push = (s: string) => {
    const v = s.trim();
    const k = v.toLowerCase();
    if (v && !seen.has(k)) {
      seen.add(k);
      suggestions.push(v);
    }
  };
  performers.forEach((p) => push(p.name));
  performers.forEach((p) => (p.aliases ?? []).forEach(push));

  return (
    <div className="alias-retry open">
      <div className="alias-row">
        <span className="alias-label">Search as:</span>
        {suggestions.map((s) => (
          <button
            key={s}
            className={"alias-chip" + (active === s ? " sel" : "")}
            onClick={() => onSearch(s)}
          >
            {s}
          </button>
        ))}
        {/* Free-text pill for a spelling none of the suggestions cover. */}
        {typing ? (
          <input
            className="alias-input"
            placeholder="other spelling…"
            autoFocus
            value={text}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && text.trim()) onSearch(text.trim());
              if (e.key === "Escape") {
                setTyping(false);
                setText("");
              }
            }}
            onBlur={() => {
              if (!text.trim()) setTyping(false);
            }}
          />
        ) : (
          <button
            className="alias-chip other"
            onClick={() => setTyping(true)}
            title="Type a spelling that isn't listed"
          >
            + other
          </button>
        )}
        {active && (
          <button
            className="alias-chip clear"
            onClick={() => {
              setText("");
              setTyping(false);
              onSearch("");
            }}
          >
            <CloseIcon size={10} /> auto
          </button>
        )}
      </div>
    </div>
  );
}

function ReleaseList({
  releases,
  grabs,
  onGrab,
  bestKey,
  gateLabels,
}: {
  releases: SceneRelease[];
  grabs: Record<string, GrabState>;
  onGrab: (
    r: SceneRelease,
    sceneIdOverride?: string,
    confOverride?: number,
    force?: boolean,
  ) => void;
  bestKey?: string | null;
  gateLabels?: Record<string, string>;
}) {
  return (
    <ul className="release-list">
      {releases.map((r) => {
        const state = grabs[releaseKey(r)] || { status: "idle" };
        const queued = state.status === "queued" || state.status === "grabbing";
        const tier = r.verified && r.confidence > 0 ? confTier(r.confidence) : "";
        const isBest = bestKey != null && releaseKey(r) === bestKey;
        const pct = Math.round(r.confidence * 100);
        const score = r.score ?? 0;
        const scoreClass = r.rejected
          ? "is-reject"
          : score > 0
            ? "pos"
            : score < 0
              ? "neg"
              : "zero";
        const scoreTitle =
          (r.score_hits || [])
            .map(
              (h) =>
                `${h.label}: ${h.points > 0 ? "+" : ""}${h.points}${h.reject ? " (reject)" : ""}`,
            )
            .join("\n") || undefined;
        return (
          <li
            key={releaseKey(r)}
            className={
              "release" + (tier ? " " + tier : "") + (isBest ? " is-best" : "")
            }
          >
            {/* Match meter — the matcher's confidence this release IS the
                viewed scene, made the left anchor instead of buried text. */}
            <div
              className="release-match"
              style={
                r.confidence > 0
                  ? ({ ["--band" as string]: confColor(r.confidence) } as React.CSSProperties)
                  : undefined
              }
              title={r.confidence > 0 ? `Matcher confidence: ${pct}%` : undefined}
            >
              {r.confidence > 0 ? (
                <>
                  <span className="rm-pct">
                    {pct}
                    <i>%</i>
                  </span>
                  <span className="rm-label">match</span>
                </>
              ) : (
                <span className="rm-na" title="Not scored against this scene">, </span>
              )}
            </div>

            <div className="release-body">
              <div className="release-title">{r.title}</div>
              <div className="release-meta">
                {(r.failed_count ?? 0) > 0 && (
                  <span
                    className="release-dead"
                    title={
                      "This exact release already failed " +
                      r.failed_count +
                      (r.failed_count === 1 ? " time" : " times") +
                      " for a content reason and will almost certainly fail again. Last: " +
                      (r.failed_reason ?? "")
                    }
                  >
                    <DeadIcon size={10} /> died {r.failed_count}x
                  </span>
                )}
                {isBest && (
                  <span
                    className="release-best"
                    title="Recommended: best quality you can actually download"
                  >
                    <StarIcon size={10} filled /> Best
                  </span>
                )}
                <ResBadge title={r.title} />
                <span>{r.indexer}</span>
                <span>·</span>
                <span>{r.protocol}</span>
                <span>·</span>
                <span>{humanSize(r.size)}</span>
                <span>·</span>
                <span className={"rel-health " + healthClass(r)}>
                  {r.protocol === "usenet"
                    ? `${r.grabs} grabs`
                    : `${r.seeders} seeders`}
                </span>
              </div>
              {!r.verified && r.best_match_title && (
                <div className="release-warn">
                  <WarnIcon size={11} /> Looks like <strong>{r.best_match_title}</strong>
                  {r.best_match_conf
                    ? ` (${(r.best_match_conf * 100).toFixed(0)}%)`
                    : ""}
                  , not the scene you&rsquo;re viewing.
                  {r.best_match_id && !queued && (
                    <button
                      className="grab-as-btn"
                      onClick={() =>
                        onGrab(r, r.best_match_id, r.best_match_conf)
                      }
                      title={`Grab this release as "${r.best_match_title}"`}
                    >
                      Grab as this <ArrowDownIcon size={11} />
                    </button>
                  )}
                </div>
              )}
              {/* The verdict's reasoning. Falls back to the plain
                  component chips when the daemon sent no explanation. */}
              {r.explain ? (
                <MatchVerdict
                  explain={r.explain}
                  reasons={r.reasons}
                  gateLabels={gateLabels}
                />
              ) : (
                r.reasons &&
                r.reasons.length > 0 && <MatchBreakdown reasons={r.reasons} />
              )}
            </div>

            {/* Quality score — the user's release-preference total, the
                differentiator among same-scene releases. A neutral 0 is
                left blank so a real +/- score (or a reject) stands out
                instead of drowning in a column of zeros. */}
            {r.rejected || score !== 0 ? (
              <div className={"release-score-stat " + scoreClass} title={scoreTitle}>
                <span className="rs-val">
                  {r.rejected ? (
                        <BlockedIcon size={11} />
                      ) : (
                        `${score > 0 ? "+" : ""}${score}`
                      )}
                </span>
                <span className="rs-label">{r.rejected ? "reject" : "score"}</span>
              </div>
            ) : (
              <div className="release-score-stat is-empty" aria-hidden="true" />
            )}

            <GrabButton
              state={state}
              onGrab={(force) => onGrab(r, undefined, undefined, force)}
            />
          </li>
        );
      })}
    </ul>
  );
}

// MatchBreakdown is the "why did this match?" expander. The matcher
// emits one reason string per scoring component ("performers: 2/2",
// "studio: match", "date: exact", "title: 0.43", "tracks: A+B"); we split
// each into a label + value chip and tint it by whether the component
// actually contributed (a hit) or not, so a glance shows what carried the
// match vs. what was missing.
const REASON_MISS = /\b(none|no-match|n\/a|missing|parse-error|off)\b/i;

function MatchBreakdown({ reasons }: { reasons: string[] }) {
  return (
    <details className="match-why">
      <summary>Match breakdown</summary>
      <ReasonChips reasons={reasons} />
    </details>
  );
}

// ReasonChips renders the matcher's per-component reason strings as tinted
// pills. Shared by the plain breakdown and the verdict panel so a scene's
// signals look the same wherever they appear.
function ReasonChips({ reasons }: { reasons: string[] }) {
  return (
    <ul className="match-why-list">
      {reasons.map((reason, i) => {
        const sep = reason.indexOf(":");
        const label = sep >= 0 ? reason.slice(0, sep).trim() : reason;
        const value = sep >= 0 ? reason.slice(sep + 1).trim() : "";
        const miss = REASON_MISS.test(value);
        return (
          <li
            key={i}
            className={"match-why-row " + (miss ? "is-miss" : "is-hit")}
          >
            <span className="match-why-label">{label}</span>
            {value && <span className="match-why-value">{value}</span>}
          </li>
        );
      })}
    </ul>
  );
}

// MatchVerdict is the "why did (or didn't) this verify?" panel.
//
// The badge alone is one bit standing in for four acceptance paths and a dozen
// thresholds, and a wrong answer looked exactly like a right one. The case
// that prompted this verified the WRONG scene at 73% with its date 1,593 days
// off, and the top three candidates were 0.013 apart. Nothing on screen could
// have shown that. So the panel says three things in order: what decided it,
// which paths were tried and what stopped each, and the ranked field with what
// every candidate scored.
//
// It expands on click and ships with the release list (no extra request): the
// daemon builds it from candidates it already had.
function MatchVerdict({
  explain,
  reasons,
  gateLabels,
}: {
  explain: MatchExplain;
  reasons?: string[];
  // Gate name → human label, sent once per response. Missing entry falls back
  // to the name, which is ugly but never blank.
  gateLabels?: Record<string, string>;
}) {
  // The body is mounted only while the panel is open.
  //
  // <details> keeps its children in the DOM whether or not it is open, and
  // there is one of these per release on a list that has no pagination and can
  // run to a few hundred rows, so the first version put the whole body of every
  // panel into the DOM at load.
  //
  // Measured, not estimated: renderToStaticMarkup of this component against a
  // five-gate / six-candidate explanation emits 108 element nodes with the body
  // mounted and 4 with it closed. At 300 releases that is 32,400 nodes against
  // 1,200. Reproduce by exporting MatchVerdict, rendering it under
  // react-dom/server with `useState(true)` and then `useState(false)`, and
  // counting /<[a-zA-Z]/g in the output.
  //
  // The summary stays mounted either way, so the row still shows its verdict
  // and hint without being opened.
  const [open, setOpen] = useState(false);

  const gates = explain.gates ?? [];
  const cands = explain.candidates ?? [];
  const pos = explain.position;
  const overrides = explain.overrides ?? [];
  const verified = explain.verified;
  const shared = explain.shared_blockers ?? [];
  const label = (name: string) => gateLabels?.[name] ?? name;

  // Collapsed line: the single most useful fact, so the common question is
  // answered without opening anything.
  //
  // forage's own rules are tested FIRST because they are the last word in the
  // daemon: they set `verified`, overriding whatever the matcher decided. Read
  // in the other order the summary contradicted itself, which on the one line
  // this feature exists to make trustworthy is worse than saying less. The
  // cases: a JAV-code accept with no candidates read "Verified / nothing to
  // compare against"; the same accept over a scene ranked third read "Verified
  // / ranked #3 of 8"; and a behind-the-scenes veto plus a matching JAV code
  // read "Verified / refused by a rule".
  const accepted = overrides.some((o) => o.verdict === "verified");
  const refused =
    overrides.some((o) => o.verdict === "refused") || !!explain.veto;
  //
  // Each rule hint is gated on the verdict it would imply, because testing
  // them purely in daemon order reintroduced the contradiction in a new
  // place. verifyReleases builds overrides in that order: the JAV-code accept
  // fires on !verified, then the pack / link-spam / image-set refusals fire on
  // verified. A JAV-coded release that also reads as a pack therefore ends up
  // with overrides [verified, refused] and a FINAL verified of false, and the
  // summary rendered "Not verified" beside "accepted by a rule". Confirmed in
  // a browser, not reasoned: titles like "SSIS-123 Mega Pack" reach it.
  //
  // So the rule is simply that the hint may never disagree with the badge.
  let hint = "";
  if (verified && accepted) hint = "accepted by a rule";
  else if (!verified && refused) hint = "refused by a rule";
  else if (verified && explain.path_label) hint = explain.path_label;
  else if (explain.note) hint = "nothing to compare against";
  else if (!pos.found) hint = "not among the candidates";
  else if (pos.rank > 1) hint = `ranked #${pos.rank} of ${pos.candidates}`;
  else hint = "no acceptance path applied";

  return (
    <details
      className="match-why match-verdict"
      open={open}
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary>
        <span className={"mv-verdict " + (verified ? "is-yes" : "is-no")}>
          {verified ? "Verified" : "Not verified"}
        </span>
        {hint && <span className="mv-hint">{hint}</span>}
      </summary>

      {open && (
        <>
      {explain.note && <p className="mv-note">{explain.note}</p>}

      {/* forage's own rules come last in the daemon and first here: when one
          fired it is the actual answer, and the matcher's reasoning below is
          only context for what it overrode. */}
      {overrides.map((o, i) => (
        <p
          key={i}
          className={
            "mv-override " + (o.verdict === "refused" ? "is-no" : "is-yes")
          }
        >
          {o.verdict === "refused" ? "Refused: " : "Accepted: "}
          {o.reason}
        </p>
      ))}

      {explain.veto && <p className="mv-override is-no">Refused: {explain.veto}</p>}

      {gates.length > 0 && (
        <>
          <div className="mv-caption">
            Ways this release could be accepted as the scene you're viewing
          </div>
          {shared.length > 0 && (
            <ul className="mv-blockers mv-shared">
              {shared.map((b, i) => (
                <li key={i}>{b}, which rules out every path below that needs it.</li>
              ))}
            </ul>
          )}
          <ul className="mv-gates">
            {gates.map((g) => {
              const own = g.blockers ?? [];
              return (
                <li
                  key={g.name}
                  className={"mv-gate " + (g.passed ? "is-pass" : "is-fail")}
                >
                  <span className="mv-gate-head">
                    <span className="mv-mark" aria-hidden="true">
                      {g.passed ? <CheckIcon size={10} /> : <CloseIcon size={10} />}
                    </span>
                    <span className="mv-gate-label">{label(g.name)}</span>
                  </span>
                  {own.length > 0 ? (
                    <ul className="mv-blockers">
                      {own.map((b, i) => (
                        <li key={i}>{b}</li>
                      ))}
                    </ul>
                  ) : (
                    // Every reason this path gave was hoisted above. Saying so
                    // beats a bare crossed-out label with nothing attached.
                    !g.passed &&
                    shared.length > 0 && (
                      <ul className="mv-blockers">
                        <li className="mv-only-shared">
                          stopped only by the reason above
                        </li>
                      </ul>
                    )
                  )}
                </li>
              );
            })}
          </ul>
        </>
      )}

      {cands.length > 0 && (
        <>
          <div className="mv-caption">
            Scenes the matcher weighed for this release name
          </div>
          <div className="mv-cands-scroll">
            <table className="mv-cands">
              <thead>
                <tr>
                  <th>#</th>
                  <th title="The matcher's overall confidence that this release is this scene">
                    Match
                  </th>
                  <th title="How much of the release name and this scene's title are the same words (0 to 1)">
                    Title fit
                  </th>
                  <th>Scene</th>
                </tr>
              </thead>
              <tbody>
                {cands.map((c) => (
                  <tr
                    key={c.scene_id}
                    className={c.is_target ? "is-target" : undefined}
                  >
                    <td className="mv-rank">{c.rank}</td>
                    <td className="mv-num">
                      {(c.confidence * 100).toFixed(0)}%
                    </td>
                    <td className="mv-num">{c.title_overlap.toFixed(2)}</td>
                    <td>
                      <span className="mv-cand-title">
                        {c.title || "(untitled)"}
                      </span>
                      {c.is_target && (
                        <span className="mv-you">the scene you're viewing</span>
                      )}
                      <span className="mv-cand-meta">
                        {[
                          c.date,
                          c.studio,
                          // The daemon caps the cast list; a compilation in
                          // the corpus carries 109 names, which used to be
                          // sent in full and rendered as one very tall row.
                          (c.cast ?? []).join(", ") +
                            (c.cast_more ? ` +${c.cast_more} more` : ""),
                        ]
                          .filter(Boolean)
                          .join(" · ")}
                        {c.date_far_off && (
                          <span className="mv-far"> date 2+ years off</span>
                        )}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}

      {reasons && reasons.length > 0 && (
        <>
          <div className="mv-caption">
            What this scene scored on, component by component
          </div>
          <ReasonChips reasons={reasons} />
        </>
      )}
        </>
      )}
    </details>
  );
}

function GrabButton({
  state,
  onGrab,
}: {
  state: GrabState;
  onGrab: (force?: boolean) => void;
}) {
  switch (state.status) {
    case "idle":
      return (
        <button className="grab-btn" onClick={() => onGrab()}>
          Grab <ArrowDownIcon size={11} />
        </button>
      );
    case "grabbing":
      return (
        <button className="grab-btn" disabled>
          Queueing…
        </button>
      );
    case "queued":
      return (
        <button className="grab-btn queued" disabled>
          Queued → {state.client}
        </button>
      );
    case "error": {
      // A disk-space preflight rejection is overridable — offer "Grab
      // anyway" (force) rather than a plain retry.
      const lowSpace = /free space/i.test(state.message);
      return (
        <button
          className="grab-btn error"
          onClick={() => onGrab(lowSpace)}
          title={state.message}
        >
          {lowSpace ? "Grab anyway" : "Failed, retry"}
        </button>
      );
    }
  }
}
