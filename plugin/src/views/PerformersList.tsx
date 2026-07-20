import SubscriptionsRow from "../SubscriptionsRow";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { filterGlyph } from "../format";
import {
  fetchPerformers,
  PerformerSort,
  performerImageURL,
  refreshPerformers,
  type Performer,
} from "../api";

const SORT_OPTIONS: { value: PerformerSort; label: string }[] = [
  { value: "scene_count", label: "Owned scene count" },
  { value: "name", label: "Name" },
  { value: "last_release", label: "Last release" },
  { value: "missing_count", label: "Missing scenes" },
];

const SORT_STORAGE_KEY = "forage.performers.sort";
const FILTER_STORAGE_KEY = "forage.performers.filter";

function loadSort(): PerformerSort {
  const stored = localStorage.getItem(SORT_STORAGE_KEY) as PerformerSort | null;
  if (stored && SORT_OPTIONS.some((o) => o.value === stored)) return stored;
  return "scene_count";
}

export default function PerformersList({
  onPick,
}: {
  onPick: (localID: string) => void;
}) {
  const [performers, setPerformers] = useState<Performer[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [favOnly, setFavOnly] = useState(false);
  const [q, setQ] = useState("");
  const [sort, setSort] = useState<PerformerSort>(loadSort);
  const [refreshing, setRefreshing] = useState(false);
  // Deployment-configured content filter ("" = all) — the same named
  // gender sets as Discover, applied in memory against each performer's
  // cached gender (the full list is already loaded; a chip toggle must
  // not cost a refetch). Persisted like sort.
  const [contentFilter, setContentFilter] = useState<string>(
    () => localStorage.getItem(FILTER_STORAGE_KEY) || "",
  );
  const [filterSets, setFilterSets] = useState<Record<string, string[]>>({});

  useEffect(() => {
    localStorage.setItem(SORT_STORAGE_KEY, sort);
  }, [sort]);
  useEffect(() => {
    localStorage.setItem(FILTER_STORAGE_KEY, contentFilter);
  }, [contentFilter]);

  // Re-fetch the cached performer list. Returns a promise so the manual
  // refresh can await it before clearing its spinner. loadSeq guards
  // against out-of-order responses: two quick sort changes fire two
  // loads, and without the guard the slower (stale) response would win
  // and display the old sort's data. Only the latest call may commit.
  const loadSeq = useRef(0);
  const load = useCallback(
    async (showSpinner = true) => {
      const seq = ++loadSeq.current;
      if (showSpinner) setLoading(true);
      try {
        const r = await fetchPerformers({ sort });
        if (seq !== loadSeq.current) return;
        setPerformers(r.performers);
        setFilterSets(r.filters ?? {});
        setError(null);
      } catch (e) {
        if (seq !== loadSeq.current) return;
        setError((e as Error).message);
      } finally {
        if (showSpinner && seq === loadSeq.current) setLoading(false);
      }
    },
    [sort],
  );

  useEffect(() => {
    void load();
  }, [load]);

  // Force an immediate server-side re-sync of the performer cache from Stash
  // (the fast pull) so a just-added performer shows up without waiting for
  // the 6h cache tick — mirrors the Grabs "Scan for downloads" button.
  async function refreshNow() {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await refreshPerformers();
      await load(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setRefreshing(false);
    }
  }

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    // A selected filter whose sets the server no longer advertises (a
    // stale chip from an old config) narrows nothing.
    const genders = contentFilter ? filterSets[contentFilter] : undefined;
    return performers.filter((p) => {
      if (favOnly && !p.favorite) return false;
      if (genders && !genders.includes(p.gender || "")) return false;
      if (!needle) return true;
      if (p.name.toLowerCase().includes(needle)) return true;
      return (p.aliases || []).some((a) => a.toLowerCase().includes(needle));
    });
  }, [performers, favOnly, q, contentFilter, filterSets]);

  if (loading) return <div className="empty">Loading performers…</div>;
  // Full-screen error only when there's nothing to show — a failed
  // refresh or re-sort against an already-loaded grid keeps the grid and
  // surfaces inline below the controls instead, so the user keeps the
  // Refresh button (their retry path).
  if (error && performers.length === 0)
    return <div className="empty error">Failed to load: {error}</div>;

  return (
    <div>
      <SubscriptionsRow kind="performer" />
      <div className="controls">
        <input
          type="text"
          placeholder="Filter performers…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <select
          className="sort-select"
          value={sort}
          onChange={(e) => setSort(e.target.value as PerformerSort)}
        >
          {SORT_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              Sort: {o.label}
            </option>
          ))}
        </select>
        <label className="check">
          <input
            type="checkbox"
            checked={favOnly}
            onChange={(e) => setFavOnly(e.target.checked)}
          />
          Favourites only
        </label>
        {Object.keys(filterSets).length > 0 && (
          <span className="discover-filters">
            {Object.keys(filterSets)
              .sort()
              .map((f) => {
                const glyph = filterGlyph(filterSets[f]);
                return (
                  <button
                    key={f}
                    className={
                      "grab-chip" +
                      (glyph ? " chip-icon" : "") +
                      (contentFilter === f ? " active chip-any" : "")
                    }
                    title={`Only show ${f} performers (deployment filter)`}
                    onClick={() =>
                      setContentFilter(contentFilter === f ? "" : f)
                    }
                  >
                    {glyph ? (
                      <span className="chip-glyph" aria-label={f}>
                        {glyph}
                      </span>
                    ) : (
                      <span className="chip-label">{f}</span>
                    )}
                  </button>
                );
              })}
          </span>
        )}
        <span className="count">
          {filtered.length} / {performers.length}
        </span>
        <button
          className="grab-adopt-btn"
          onClick={refreshNow}
          disabled={refreshing}
          title="Re-sync the performer list from Stash now — picks up newly added performers"
        >
          {refreshing ? "Refreshing…" : "↻ Refresh"}
        </button>
      </div>
      {error && (
        <div className="perf-list-err">Refresh failed: {error}</div>
      )}
      <div className="performer-grid">
        {filtered.map((p) => (
          <PerformerCard
            key={p.stash_id}
            p={p}
            onPick={() => onPick(p.stash_id)}
          />
        ))}
      </div>
    </div>
  );
}

function PerformerCard({
  p,
  onPick,
}: {
  p: Performer;
  onPick: () => void;
}) {
  const imgURL = performerImageURL(p.stash_id);
  const hasStashDBData = p.total_stashdb_scenes > 0;
  const missing = Math.max(0, p.total_stashdb_scenes - p.owned_scenes_count);
  const completion = hasStashDBData
    ? Math.min(
        100,
        Math.round((p.owned_scenes_count / p.total_stashdb_scenes) * 100),
      )
    : null;
  const lastRelease =
    p.last_release_unix > 0
      ? new Date(p.last_release_unix * 1000).toISOString().slice(0, 10)
      : null;
  return (
    <button
      className={"performer-card" + (p.favorite ? " fav" : "")}
      onClick={onPick}
    >
      {imgURL ? (
        <img
          className="perf-img"
          src={imgURL}
          alt=""
          loading="lazy"
          onError={(e) => {
            (e.currentTarget as HTMLImageElement).style.display = "none";
          }}
        />
      ) : (
        <div className="perf-img perf-img-empty">{p.name.slice(0, 1)}</div>
      )}
      {p.favorite ? <HeartIcon /> : null}
      <div className="perf-scrim">
        <div className="perf-name">{p.name}</div>
        {/* StashDB-derived stats (missing, owned/total, last release)
            only appear once the scene cache has data for this performer
            — a cross-id + the 12h refresh. Otherwise just the raw
            library count. */}
        <div className="perf-stats">
          {hasStashDBData ? (
            <>
              <span className="perf-missing">
                {missing > 0 ? `${missing} missing` : "complete"}
              </span>
              {lastRelease && (
                <>
                  <span className="sep">·</span>
                  <span>{lastRelease}</span>
                </>
              )}
            </>
          ) : (
            <span>{p.scene_count} in library</span>
          )}
        </div>
      </div>
      {completion != null && (
        <div className="perf-bar" title={`${completion}% of StashDB scenes`}>
          <div className="perf-bar-fill" style={{ width: `${completion}%` }} />
        </div>
      )}
    </button>
  );
}

/* Solid heart glyph — Font Awesome 6 "heart" path, MIT licensed.
   Same shape Stash uses for its favourite button, so this reads the
   same as refract's pink-glow heart on regular Stash performer cards. */
function HeartIcon() {
  return (
    <span className="heart-icon" aria-label="Favourite">
      <svg viewBox="0 0 512 512" aria-hidden="true">
        <path d="M47.6 300.4 228.3 469.1c7.5 7 17.4 10.9 27.7 10.9s20.2-3.9 27.7-10.9l180.7-168.7C495 273.7 512 234.7 512 193.8v-5.8c0-68.2-49.4-126.3-116.6-137.4-44.5-7.4-89.8 7.2-121.4 39l-18 18-18-18c-31.6-31.8-76.9-46.4-121.4-39C49.4 61.6 0 119.7 0 187.9v5.8c0 40.9 17 79.9 47.6 108.6z" />
      </svg>
    </span>
  );
}
