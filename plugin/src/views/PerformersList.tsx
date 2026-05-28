import { useEffect, useMemo, useState } from "react";
import {
  fetchPerformers,
  PerformerSort,
  performerImageURL,
  type Performer,
} from "../api";

const SORT_OPTIONS: { value: PerformerSort; label: string }[] = [
  { value: "scene_count", label: "Owned scene count" },
  { value: "name", label: "Name" },
  { value: "last_release", label: "Last release" },
  { value: "missing_count", label: "Missing scenes" },
];

const SORT_STORAGE_KEY = "forage.performers.sort";

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

  useEffect(() => {
    localStorage.setItem(SORT_STORAGE_KEY, sort);
  }, [sort]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    fetchPerformers({ sort })
      .then((r) => {
        if (cancelled) return;
        setPerformers(r.performers);
        setError(null);
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
  }, [sort]);

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return performers.filter((p) => {
      if (favOnly && !p.favorite) return false;
      if (!needle) return true;
      if (p.name.toLowerCase().includes(needle)) return true;
      return (p.aliases || []).some((a) => a.toLowerCase().includes(needle));
    });
  }, [performers, favOnly, q]);

  if (loading) return <div className="empty">Loading performers…</div>;
  if (error) return <div className="empty error">Failed to load: {error}</div>;

  return (
    <div>
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
        <span className="count">
          {filtered.length} / {performers.length}
        </span>
      </div>
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
