import { useState } from "react";
import { addWatch, deleteWatch } from "./api";

// Shared watch control — track a StashDB scene for releases. One click; no
// quality target (the matcher surfaces the best release by your preference
// ranking, any quality, and quality floors live in your release reject rules).
// Used by the missing-scenes + discover cards (variant "overlay": a bookmark
// chip in the thumb's bottom strip) and the scene-releases hero (variant
// "inline": "watch this if nothing here is good enough").

export interface WatchScene {
  stashdb_id: string;
  // Stash-box endpoint that issued stashdb_id. Omitted = StashDB. Carried so
  // the daemon resolves the id against the box it came from; the ids are only
  // unique within a box.
  source?: string;
  title?: string;
  date?: string;
  studio?: string;
  image_url?: string;
}

export default function WatchControl({
  scene,
  performerName,
  performerId,
  batch,
  initialStatus = "",
  variant = "overlay",
}: {
  scene: WatchScene;
  performerName?: string;
  performerId?: string;
  // Stable group for this add: subject pages pass their per-subject
  // batch so one-by-one watches join the same Watching-tab group a
  // multi-select would create. Omitted = ungrouped single track.
  batch?: { id: string; label?: string };
  initialStatus?: string;
  variant?: "overlay" | "inline";
}) {
  const [watch, setWatch] = useState(initialStatus);
  const [busy, setBusy] = useState(false);

  const track = async () => {
    setBusy(true);
    try {
      await addWatch({
        stashdb_id: scene.stashdb_id,
        source: scene.source,
        title: scene.title ?? "",
        date: scene.date,
        studio: scene.studio,
        image_url: scene.image_url,
        performer_name: performerName,
        performer_id: performerId,
        batch_id: batch?.id,
        batch_label: batch?.label,
      });
      setWatch("watching");
    } catch {
      /* leave state as-is */
    } finally {
      setBusy(false);
    }
  };
  const untrack = async () => {
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
      {/* One chip that changes colour in place: grey to watch, amber while
          watching, green when a release is ready. The refinement pass split
          this into a state tag top-left and an action pill bottom-right, on
          the argument that colour should not carry the signal alone. Reverted
          on use: the colour change IS the signal here, it happens on the
          control you just pressed, and moving it to the far corner made a
          state change look like two different controls appearing.

          What survives from the pass: the chip is permanent rather than
          revealed on hover, because touch has no hover. */}
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
      ) : (
        <button
          className="watch-chip"
          disabled={busy}
          onClick={track}
          title="Watch for releases — surfaces the best by your preferences"
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
