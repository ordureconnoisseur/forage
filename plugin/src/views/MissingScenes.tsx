import { useEffect, useState } from "react";
import {
  addWatch,
  deleteWatch,
  fetchMissing,
  type MissingResponse,
  type MissingScene,
  type WatchTarget,
} from "../api";

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
          <span className="ms-select-count">
            {selected.size} selected
          </span>
          <button
            className="ms-select-grab"
            disabled={selected.size === 0}
            onClick={() =>
              onGrabSelected(performerId, Array.from(selected))
            }
          >
            Grab {selected.size} selected →
          </button>
        </div>
      )}
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
  // Local watch state so the Track control updates immediately without a
  // full reload. Seeded from the server's watch_status.
  const [watch, setWatch] = useState<string>(s.watch_status || "");
  const [picking, setPicking] = useState(false);
  const [busy, setBusy] = useState(false);

  const track = async (target: WatchTarget) => {
    setPicking(false);
    setBusy(true);
    try {
      await addWatch({
        stashdb_id: s.stashdb_id,
        title: s.title,
        date: s.date,
        studio: s.studio,
        image_url: s.image_url,
        performer_name: performerName,
        performer_id: performerId,
        target,
      });
      setWatch("watching");
    } catch {
      /* leave state as-is */
    } finally {
      setBusy(false);
    }
  };
  const untrack = async () => {
    setPicking(false);
    setBusy(true);
    try {
      await deleteWatch(s.stashdb_id);
      setWatch("");
    } catch {
      /* noop */
    } finally {
      setBusy(false);
    }
  };

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
        {/* Watch control — expands inline (no popup) so it can't be
            clipped by the thumb's overflow. stopPropagation keeps clicks
            off the card's navigate action. Hidden in select mode. */}
        {!selecting && (
          <div className="scene-watch" onClick={(e) => e.stopPropagation()}>
            {watch === "available" ? (
              <span
                className="watch-chip is-available"
                title="A release was found — open the Watching tab to grab it"
              >
                <BookmarkGlyph filled />
                Ready
              </span>
            ) : watch === "watching" ? (
              <button
                className="watch-chip is-watching"
                disabled={busy}
                onClick={untrack}
                title="Watching for releases — click to stop"
              >
                <BookmarkGlyph filled />
                Watching
              </button>
            ) : picking ? (
              <div className="watch-picker" role="menu" aria-label="Watch at quality">
                {(["any", "1080p", "4k", "720p"] as WatchTarget[]).map((t) => (
                  <button
                    key={t}
                    className="watch-q"
                    disabled={busy}
                    onClick={() => track(t)}
                  >
                    {t === "any" ? "Any" : t === "4k" ? "4K" : t}
                  </button>
                ))}
                <button
                  className="watch-q watch-cancel"
                  onClick={() => setPicking(false)}
                  aria-label="Cancel"
                >
                  ×
                </button>
              </div>
            ) : (
              <button
                className="watch-chip"
                disabled={busy}
                onClick={() => setPicking(true)}
                title="Watch for releases at a chosen quality"
              >
                <BookmarkGlyph />
                Watch
              </button>
            )}
          </div>
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

// BookmarkGlyph — the watch metaphor. Outline when idle, filled once the
// scene is being watched. Sized to sit inline with the chip label.
function BookmarkGlyph({ filled = false }: { filled?: boolean }) {
  return (
    <svg
      viewBox="0 0 24 24"
      width="11"
      height="11"
      aria-hidden="true"
      fill={filled ? "currentColor" : "none"}
      stroke="currentColor"
      strokeWidth="2.2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M6 3h12a1 1 0 0 1 1 1v17l-7-4-7 4V4a1 1 0 0 1 1-1z" />
    </svg>
  );
}
