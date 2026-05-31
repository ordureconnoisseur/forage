import { useEffect, useState } from "react";
import {
  addWatch,
  fetchMissing,
  type MissingResponse,
  type MissingScene,
  type WatchTarget,
} from "../api";
import WatchControl from "../WatchControl";

// grabStatusLabel maps a raw grab status to a short card badge label.
function grabStatusLabel(status: string): string {
  switch (status) {
    case "queued":
      return "⏳ Queued";
    case "downloading":
      return "↓ Downloading";
    case "completed":
    case "placed":
      return "↓ Downloaded";
    case "scanned":
      return "⟳ Scanning";
    default:
      return "↓ In progress";
  }
}

export default function MissingScenes({
  performerId,
  onPickScene,
  onCollection,
  onGrabSelected,
}: {
  performerId: string;
  // Receives the performer name too so the scene-releases page can
  // pass it through to /grab — the placer needs to know which library
  // folder to drop the file in.
  onPickScene: (stashDBID: string, performerName: string) => void;
  // Launches "complete the collection" mode for this performer.
  onCollection: (performerId: string) => void;
  // Launches the collection flow scoped to a hand-picked scene subset.
  onGrabSelected: (performerId: string, sceneIds: string[]) => void;
}) {
  const [data, setData] = useState<MissingResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Multi-select mode: when on, cards toggle selection instead of
  // navigating. selected holds the chosen StashDB ids.
  const [selecting, setSelecting] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  // Bulk-watch: pick a quality for all selected scenes at once.
  const [watchPicking, setWatchPicking] = useState(false);
  const [watchBusy, setWatchBusy] = useState(false);
  const [watchedMsg, setWatchedMsg] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setData(null);
    setError(null);
    // Reset selection when switching performers.
    setSelecting(false);
    setSelected(new Set());
    fetchMissing(performerId)
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
  }, [performerId]);

  if (loading) return <div className="empty">Loading missing scenes…</div>;
  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!data) return null;

  const toggleSelected = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const exitSelect = () => {
    setSelecting(false);
    setSelected(new Set());
    setWatchPicking(false);
  };

  // Watch every selected scene at one quality target, in parallel.
  const watchSelected = async (target: WatchTarget) => {
    setWatchPicking(false);
    setWatchBusy(true);
    const chosen = data.missing.filter((s) => selected.has(s.stashdb_id));
    await Promise.all(
      chosen.map((s) =>
        addWatch({
          stashdb_id: s.stashdb_id,
          title: s.title,
          date: s.date,
          studio: s.studio,
          image_url: s.image_url,
          performer_name: data.performer.name,
          performer_id: data.performer.local_id,
          target,
        }).catch(() => {}),
      ),
    );
    setWatchBusy(false);
    const n = chosen.length;
    exitSelect();
    setWatchedMsg(`Watching ${n} scene${n === 1 ? "" : "s"} ✓`);
    window.setTimeout(() => setWatchedMsg(null), 3500);
  };

  const allIds = data.missing.map((s) => s.stashdb_id);
  const allSelected = selected.size === allIds.length && allIds.length > 0;

  return (
    <div>
      <div className="page-header page-header-row">
        <div>
          <h2>{data.performer.name}</h2>
          <div className="meta">
            {data.total_scenes} on StashDB · {data.owned_count} in library ·{" "}
            <strong>{data.missing.length} missing</strong>
          </div>
        </div>
        {data.missing.length > 0 &&
          (selecting ? (
            <div className="ms-select-actions">
              <button
                className="ms-select-toggle"
                onClick={() =>
                  setSelected(allSelected ? new Set() : new Set(allIds))
                }
              >
                {allSelected ? "Clear all" : "Select all"}
              </button>
              <button className="ms-select-cancel" onClick={exitSelect}>
                Cancel
              </button>
            </div>
          ) : (
            <div className="ms-select-actions">
              <button
                className="ms-select-toggle"
                onClick={() => setSelecting(true)}
              >
                Select
              </button>
              <button
                className="collection-cta"
                onClick={() => onCollection(performerId)}
              >
                Complete collection →
              </button>
            </div>
          ))}
      </div>
      {data.missing.length === 0 ? (
        <div className="empty">
          You have every StashDB scene for this performer in your library.
        </div>
      ) : (
        <div className="scene-grid">
          {data.missing.map((s) => (
            <SceneCard
              key={s.stashdb_id}
              s={s}
              performerName={data.performer.name}
              performerId={data.performer.local_id}
              selecting={selecting}
              selected={selected.has(s.stashdb_id)}
              onPick={() =>
                selecting
                  ? toggleSelected(s.stashdb_id)
                  : onPickScene(s.stashdb_id, data.performer.name)
              }
            />
          ))}
        </div>
      )}
      {selecting && (
        <div className="ms-select-bar">
          <span className="ms-select-count">{selected.size} selected</span>
          {watchPicking ? (
            <div className="ms-watch-picker" role="menu">
              <span className="ms-watch-picker-label">Watch at:</span>
              {(["any", "4k", "1080p", "720p", "480p"] as WatchTarget[]).map(
                (t) => (
                  <button
                    key={t}
                    disabled={watchBusy}
                    onClick={() => watchSelected(t)}
                  >
                    {t === "any" ? "Any" : t === "4k" ? "4K" : t === "480p" ? "SD" : t}
                  </button>
                ),
              )}
              <button
                className="ms-watch-cancel"
                onClick={() => setWatchPicking(false)}
                aria-label="Cancel"
              >
                ×
              </button>
            </div>
          ) : (
            <button
              className="ms-select-watch"
              disabled={selected.size === 0 || watchBusy}
              onClick={() => setWatchPicking(true)}
              title="Watch all selected scenes for releases"
            >
              {watchBusy ? "Watching…" : `Watch ${selected.size} selected ▾`}
            </button>
          )}
          <button
            className="ms-select-grab"
            disabled={selected.size === 0}
            onClick={() => onGrabSelected(performerId, Array.from(selected))}
          >
            Grab {selected.size} selected →
          </button>
        </div>
      )}
      {watchedMsg && <div className="ms-toast">{watchedMsg}</div>}
    </div>
  );
}

function SceneCard({
  s,
  performerName,
  performerId,
  selecting,
  selected,
  onPick,
}: {
  s: MissingScene;
  performerName: string;
  performerId: string;
  selecting: boolean;
  selected: boolean;
  onPick: () => void;
}) {

  return (
    <div
      className={
        "scene-card" +
        (selecting ? " selectable" : "") +
        (selected ? " selected" : "")
      }
      role="button"
      tabIndex={0}
      onClick={onPick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") onPick();
      }}
      aria-pressed={selecting ? selected : undefined}
    >
      {selecting && (
        <span className="scene-check" aria-hidden="true">
          {selected ? "✓" : ""}
        </span>
      )}
      <div className="scene-thumb">
        {s.image_url ? (
          <img
            src={s.image_url}
            alt=""
            loading="lazy"
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.display = "none";
            }}
          />
        ) : null}
        {s.grab_status && (
          <span
            className={"scene-grab-badge gs-" + s.grab_status}
            title={`Grab in progress — ${s.grab_status}`}
          >
            {grabStatusLabel(s.grab_status)}
          </span>
        )}
        {/* Watch control — hidden in select mode (the card toggles
            selection then, not navigation). */}
        {!selecting && (
          <WatchControl
            scene={{
              stashdb_id: s.stashdb_id,
              title: s.title,
              date: s.date,
              studio: s.studio,
              image_url: s.image_url,
            }}
            performerName={performerName}
            performerId={performerId}
            initialStatus={s.watch_status || ""}
            variant="overlay"
          />
        )}
      </div>
      <div className="scene-info">
        <div className="title">{s.title || "(untitled)"}</div>
        <div className="meta">
          {s.date && <span>{s.date}</span>}
          {s.date && s.studio && <span> · </span>}
          {s.studio && <span>{s.studio}</span>}
        </div>
        {s.performers && s.performers.length > 0 ? (
          <div className="performers">
            {s.performers
              .map((p) => (p.as && p.as !== p.name ? `${p.name} (as ${p.as})` : p.name))
              .join(", ")}
          </div>
        ) : null}
      </div>
    </div>
  );
}

