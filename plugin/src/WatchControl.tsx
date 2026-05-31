import { useState } from "react";
import { addWatch, deleteWatch, type WatchTarget } from "./api";

// Shared watch control — track a StashDB scene for releases at a chosen
// quality. Used by the missing-scenes + discover cards (variant
// "overlay": a bookmark chip in the thumb's bottom strip that expands
// inline into a quality bar, so it can't be clipped) and the
// scene-releases hero (variant "inline": a normal-flow button for "watch
// this if nothing here is good enough").

const TARGETS: WatchTarget[] = ["any", "4k", "1080p", "720p", "480p"];
function targetLabel(t: WatchTarget): string {
  return t === "any" ? "Any" : t === "4k" ? "4K" : t === "480p" ? "SD" : t;
}

export interface WatchScene {
  stashdb_id: string;
  title?: string;
  date?: string;
  studio?: string;
  image_url?: string;
}

export default function WatchControl({
  scene,
  performerName,
  performerId,
  initialStatus = "",
  variant = "overlay",
}: {
  scene: WatchScene;
  performerName?: string;
  performerId?: string;
  initialStatus?: string;
  variant?: "overlay" | "inline";
}) {
  const [watch, setWatch] = useState(initialStatus);
  const [picking, setPicking] = useState(false);
  const [busy, setBusy] = useState(false);

  const track = async (target: WatchTarget) => {
    setPicking(false);
    setBusy(true);
    try {
      await addWatch({
        stashdb_id: scene.stashdb_id,
        title: scene.title ?? "",
        date: scene.date,
        studio: scene.studio,
        image_url: scene.image_url,
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
      await deleteWatch(scene.stashdb_id);
      setWatch("");
    } catch {
      /* noop */
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      className={variant === "overlay" ? "scene-watch" : "watch-inline"}
      onClick={(e) => e.stopPropagation()}
    >
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
          {TARGETS.map((t) => (
            <button
              key={t}
              className="watch-q"
              disabled={busy}
              onClick={() => track(t)}
            >
              {targetLabel(t)}
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
  );
}

// BookmarkGlyph — the watch metaphor. Outline when idle, filled once the
// scene is being watched.
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
