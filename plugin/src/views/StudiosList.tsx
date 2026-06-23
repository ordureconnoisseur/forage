import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  fetchStudios,
  refreshStudios,
  type Studio,
  type StudioSort,
} from "../api";

const SORT_OPTIONS: { value: StudioSort; label: string }[] = [
  { value: "scene_count", label: "Owned scene count" },
  { value: "name", label: "Name" },
  { value: "last_release", label: "Last release" },
  { value: "missing_count", label: "Missing scenes" },
];

const SORT_STORAGE_KEY = "forage.studios.sort";

function loadSort(): StudioSort {
  const stored = localStorage.getItem(SORT_STORAGE_KEY) as StudioSort | null;
  if (stored && SORT_OPTIONS.some((o) => o.value === stored)) return stored;
  return "scene_count";
}

// StudiosList mirrors PerformersList: a browsable/searchable grid of the
// library's studios with completion bars, clicking through to the studio's
// missing/owned/dupes view (the same MissingScenes component, studio subject).
export default function StudiosList({
  onPick,
}: {
  // The studio's navigation id is its StashDB cross-id (or "stash:<id>").
  onPick: (studioId: string) => void;
}) {
  const [studios, setStudios] = useState<Studio[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [favOnly, setFavOnly] = useState(false);
  const [q, setQ] = useState("");
  const [sort, setSort] = useState<StudioSort>(loadSort);
  const [refreshing, setRefreshing] = useState(false);

  useEffect(() => {
    localStorage.setItem(SORT_STORAGE_KEY, sort);
  }, [sort]);

  const loadSeq = useRef(0);
  const load = useCallback(
    async (showSpinner = true) => {
      const seq = ++loadSeq.current;
      if (showSpinner) setLoading(true);
      try {
        const r = await fetchStudios({ sort });
        if (seq !== loadSeq.current) return;
        setStudios(r.studios);
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

  async function refreshNow() {
    if (refreshing) return;
    setRefreshing(true);
    try {
      await refreshStudios();
      await load(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setRefreshing(false);
    }
  }

  const filtered = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return studios.filter((st) => {
      if (favOnly && !st.favorite) return false;
      if (!needle) return true;
      if (st.name.toLowerCase().includes(needle)) return true;
      return (st.aliases || []).some((a) => a.toLowerCase().includes(needle));
    });
  }, [studios, favOnly, q]);

  if (loading) return <div className="empty">Loading studios…</div>;
  if (error && studios.length === 0)
    return <div className="empty error">Failed to load: {error}</div>;

  return (
    <div>
      <div className="controls">
        <input
          type="text"
          placeholder="Filter studios…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <select
          className="sort-select"
          value={sort}
          onChange={(e) => setSort(e.target.value as StudioSort)}
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
          {filtered.length} / {studios.length}
        </span>
        <button
          className="grab-adopt-btn"
          onClick={refreshNow}
          disabled={refreshing}
          title="Re-sync the studio list from Stash now — picks up newly added studios"
        >
          {refreshing ? "Refreshing…" : "↻ Refresh"}
        </button>
      </div>
      {error && <div className="perf-list-err">Refresh failed: {error}</div>}
      <div className="performer-grid">
        {filtered.map((st) => (
          <StudioCard
            key={st.stashdb_id}
            s={st}
            onPick={() => onPick(st.stashdb_id)}
          />
        ))}
      </div>
    </div>
  );
}

function StudioCard({ s, onPick }: { s: Studio; onPick: () => void }) {
  const hasStashDBData = s.total_stashdb_scenes > 0;
  const missing = Math.max(0, s.total_stashdb_scenes - s.owned_scenes_count);
  const completion = hasStashDBData
    ? Math.min(
        100,
        Math.round((s.owned_scenes_count / s.total_stashdb_scenes) * 100),
      )
    : null;
  const lastRelease =
    s.last_release_unix > 0
      ? new Date(s.last_release_unix * 1000).toISOString().slice(0, 10)
      : null;
  return (
    <button
      className={"performer-card studio-card" + (s.favorite ? " fav" : "")}
      onClick={onPick}
    >
      <div className="perf-img perf-img-empty studio-img">
        {s.name.slice(0, 1)}
      </div>
      <div className="perf-scrim">
        <div className="perf-name">{s.name}</div>
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
            <span>{s.scene_count} in library</span>
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
