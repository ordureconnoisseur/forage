import { useEffect, useState } from "react";
import {
  fetchSceneReleases,
  postGrab,
  type SceneRelease,
  type SceneReleasesResponse,
} from "../api";
import { ResBadge } from "../ResBadge";
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
  if (/\b1080p?\b/.test(t)) return 1080;
  if (/\b720p?\b/.test(t)) return 720;
  if (/\b480p?\b/.test(t)) return 480;
  return 0;
}

// releaseKey is a stable per-release identity for React keys + grab
// state. download_url alone isn't safe: magnet-only indexers (TPB) can
// share an empty download_url, which would collapse unrelated rows into
// one state bucket (clicking one appears to grab/fail them all).
function releaseKey(r: SceneRelease): string {
  return r.download_url || r.info_url || r.title;
}

function sortReleases(rels: SceneRelease[], sort: ReleaseSort): SceneRelease[] {
  const out = [...rels];
  out.sort((a, b) => {
    if (sort === "quality") {
      const d = resolutionRank(b.title) - resolutionRank(a.title);
      if (d !== 0) return d;
      return b.popularity - a.popularity;
    }
    if (sort === "match") {
      if (b.confidence !== a.confidence) return b.confidence - a.confidence;
      // tie on match → prefer higher quality, then availability
      const d = resolutionRank(b.title) - resolutionRank(a.title);
      if (d !== 0) return d;
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
  }, [sceneId, performerName, alias]);

  if (loading) return <div className="empty">Searching for releases…</div>;
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

  // grab queues a release. sceneIdOverride lets the user grab an
  // unverified release AS the scene the matcher actually thinks it is
  // (the "Looks like X" pick) — so forage predicts/identifies against the
  // right StashDB scene, not the one being viewed. confOverride carries
  // that pick's confidence for the grab record.
  const grab = async (
    rel: SceneRelease,
    sceneIdOverride?: string,
    confOverride?: number,
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
              View on StashDB ↗
            </a>
          </div>
        </div>
      </div>

      <AliasRetry
        active={alias}
        performers={data.scene.performers.map((p) => p.name)}
        onSearch={(a) => setAlias(a || null)}
      />

      {verified.length === 0 && unverified.length === 0 ? (
        <div className="empty">
          No releases found{alias ? ` for "${alias}"` : " for this scene"}.
          {!alias && (
            <div className="empty-hint">
              Trackers sometimes list a different name spelling — try
              searching another alias above.
            </div>
          )}
        </div>
      ) : (
        <>
          <div className="release-controls">
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
          </div>
          {verified.length > 0 && (
            <section>
              <h3 className="section-header">Verified ({verified.length})</h3>
              <ReleaseList releases={verified} grabs={grabs} onGrab={grab} />
            </section>
          )}
          {unverified.length > 0 && (
            <section>
              <h3
                className="section-header collapsible"
                onClick={() => setShowUnverified((v) => !v)}
              >
                {showUnverified ? "▼" : "▶"} Unverified ({unverified.length}) — different
                scenes that share a title token
              </h3>
              {showUnverified && (
                <ReleaseList releases={unverified} grabs={grabs} onGrab={grab} />
              )}
            </section>
          )}
        </>
      )}
    </div>
  );
}

// AliasRetry lets the user re-run the release search under a specific
// name spelling — for when a tracker indexed the release under an alias
// the automatic names (library name + StashDB spellings) didn't cover.
// Offers the scene's own performer names as one-click chips, plus a free
// text field.
function AliasRetry({
  active,
  performers,
  onSearch,
}: {
  active: string | null;
  performers: string[];
  onSearch: (alias: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");

  if (!open && !active) {
    return (
      <div className="alias-retry">
        <button className="alias-toggle" onClick={() => setOpen(true)}>
          Search a different name…
        </button>
      </div>
    );
  }

  return (
    <div className="alias-retry open">
      <div className="alias-row">
        <span className="alias-label">Search as:</span>
        {performers.map((p) => (
          <button
            key={p}
            className={"alias-chip" + (active === p ? " sel" : "")}
            onClick={() => onSearch(p)}
          >
            {p}
          </button>
        ))}
        <input
          className="alias-input"
          placeholder="other spelling…"
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && text.trim()) onSearch(text.trim());
          }}
        />
        {active && (
          <button
            className="alias-chip clear"
            onClick={() => {
              setText("");
              onSearch("");
            }}
          >
            ✕ auto
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
}: {
  releases: SceneRelease[];
  grabs: Record<string, GrabState>;
  onGrab: (r: SceneRelease, sceneIdOverride?: string, confOverride?: number) => void;
}) {
  return (
    <ul className="release-list">
      {releases.map((r) => {
        const state = grabs[releaseKey(r)] || { status: "idle" };
        const queued = state.status === "queued" || state.status === "grabbing";
        const tier = r.verified && r.confidence > 0 ? confTier(r.confidence) : "";
        return (
          <li key={releaseKey(r)} className={"release" + (tier ? " " + tier : "")}>
            <div className="release-body">
              <div className="release-title">{r.title}</div>
              <div className="release-meta">
                <ResBadge title={r.title} />
                <span>{r.indexer}</span>
                <span>·</span>
                <span>{r.protocol}</span>
                <span>·</span>
                <span>{humanSize(r.size)}</span>
                <span>·</span>
                <span>
                  {r.protocol === "usenet"
                    ? `${r.grabs} grabs`
                    : `${r.seeders} seeders`}
                </span>
                {r.confidence > 0 && (
                  <>
                    <span>·</span>
                    <span>match {r.confidence.toFixed(2)}</span>
                  </>
                )}
                {(r.score_hits?.length || r.rejected) && (
                  <>
                    <span>·</span>
                    <span
                      className={
                        "release-score " +
                        (r.rejected
                          ? "is-reject"
                          : (r.score ?? 0) >= 0
                            ? "pos"
                            : "neg")
                      }
                      title={(r.score_hits || [])
                        .map((h) => `${h.label}: ${h.points > 0 ? "+" : ""}${h.points}${h.reject ? " (reject)" : ""}`)
                        .join("\n")}
                    >
                      {r.rejected
                        ? "⛔ rejected"
                        : `score ${(r.score ?? 0) > 0 ? "+" : ""}${r.score ?? 0}`}
                    </span>
                  </>
                )}
              </div>
              {!r.verified && r.best_match_title && (
                <div className="release-warn">
                  ⚠ Looks like <strong>{r.best_match_title}</strong>
                  {r.best_match_conf
                    ? ` (${(r.best_match_conf * 100).toFixed(0)}%)`
                    : ""}{" "}
                  — not the scene you're viewing.
                  {r.best_match_id && !queued && (
                    <button
                      className="grab-as-btn"
                      onClick={() =>
                        onGrab(r, r.best_match_id, r.best_match_conf)
                      }
                      title={`Grab this release as "${r.best_match_title}"`}
                    >
                      Grab as this ↓
                    </button>
                  )}
                </div>
              )}
              {r.reasons && r.reasons.length > 0 && (
                <MatchBreakdown reasons={r.reasons} />
              )}
            </div>
            <GrabButton state={state} onClick={() => onGrab(r)} />
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
      <summary>Why this match?</summary>
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
    </details>
  );
}

function GrabButton({ state, onClick }: { state: GrabState; onClick: () => void }) {
  switch (state.status) {
    case "idle":
      return (
        <button className="grab-btn" onClick={onClick}>
          Grab ↓
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
    case "error":
      return (
        <button
          className="grab-btn error"
          onClick={onClick}
          title={state.message}
        >
          Failed — retry
        </button>
      );
  }
}
