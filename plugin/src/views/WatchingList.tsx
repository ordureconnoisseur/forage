import { useEffect, useRef, useState } from "react";
import {
  deleteWatch,
  dismissWatch,
  fetchWatches,
  grabWatch,
  type Watch,
} from "../api";
import { humanSize } from "../format";

// Poll cadence: the watch loop runs server-side on a 30m cadence, so the
// list changes slowly — a relaxed poll keeps the "available" badge fresh
// without hammering.
const POLL_MS = 30000;

export default function WatchingList({
  onPickScene,
}: {
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [watches, setWatches] = useState<Watch[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const timer = useRef<number | undefined>(undefined);
  // Grab-all over the Available section: one click queues every found
  // release instead of a Grab per card.
  const [grabAllBusy, setGrabAllBusy] = useState(false);
  const [grabAllMsg, setGrabAllMsg] = useState<string | null>(null);

  const load = async () => {
    try {
      const r = await fetchWatches();
      setWatches(r.watches);
      setError(null);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      if (cancelled) return;
      await load();
      timer.current = window.setTimeout(tick, POLL_MS);
    };
    void tick();
    return () => {
      cancelled = true;
      if (timer.current) clearTimeout(timer.current);
    };
  }, []);

  if (error && !watches)
    return <div className="empty error">Failed to load watches: {error}</div>;
  if (!watches) return <div className="empty">Loading…</div>;
  if (watches.length === 0)
    return (
      <div className="empty">
        Not watching any scenes. Hit <b>Track ▾</b> on a scene card to be told
        when a release at your chosen quality shows up — the server checks in
        the background, and you grab it here. Nothing is grabbed automatically.
      </div>
    );

  const available = watches.filter((w) => w.status === "available");
  const watching = watches.filter((w) => w.status !== "available");

  const grabAll = async () => {
    setGrabAllBusy(true);
    const targets = available.map((w) => w.stashdb_id);
    const results = await Promise.allSettled(
      targets.map((id) => grabWatch(id)),
    );
    const failed = results.filter((r) => r.status === "rejected").length;
    setGrabAllBusy(false);
    setGrabAllMsg(
      failed === 0
        ? `Queued ${targets.length} grab${targets.length === 1 ? "" : "s"} ✓`
        : `Queued ${targets.length - failed}, ${failed} failed — they stay listed for a retry`,
    );
    window.setTimeout(() => setGrabAllMsg(null), 4500);
    await load();
  };

  return (
    <div>
      <div className="page-header">
        <h2>Watching</h2>
        <div className="meta">
          {available.length > 0 && (
            <strong>{available.length} ready to grab</strong>
          )}
          {available.length > 0 && watching.length > 0 && " · "}
          {watching.length > 0 && `${watching.length} watching`}
        </div>
      </div>

      {available.length > 0 && (
        <section>
          <div className="watch-available-head">
            <h3 className="section-header">Available ({available.length})</h3>
            <button
              className="watch-grab"
              disabled={grabAllBusy}
              onClick={grabAll}
              title="Queue every found release for download"
            >
              {grabAllBusy
                ? "Grabbing…"
                : `Grab all ${available.length} ↓`}
            </button>
          </div>
          <ul className="watch-list">
            {available.map((w) => (
              <WatchCard
                key={w.stashdb_id}
                w={w}
                onChanged={load}
                onPickScene={onPickScene}
              />
            ))}
          </ul>
        </section>
      )}

      {grabAllMsg && <div className="ms-toast">{grabAllMsg}</div>}

      {watching.length > 0 && (
        <section>
          <h3 className="section-header">Watching ({watching.length})</h3>
          <ul className="watch-list">
            {watching.map((w) => (
              <WatchCard
                key={w.stashdb_id}
                w={w}
                onChanged={load}
                onPickScene={onPickScene}
              />
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

function WatchCard({
  w,
  onChanged,
  onPickScene,
}: {
  w: Watch;
  onChanged: () => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [grabbed, setGrabbed] = useState(false);

  const grab = async () => {
    setBusy(true);
    try {
      await grabWatch(w.stashdb_id);
      setGrabbed(true);
      onChanged();
    } catch {
      setBusy(false);
    }
  };
  const remove = async () => {
    setBusy(true);
    try {
      await deleteWatch(w.stashdb_id);
      onChanged();
    } catch {
      setBusy(false);
    }
  };
  // Reject this find but keep watching — ignores this exact release and
  // flips back to watching for a different one.
  const dismiss = async () => {
    setBusy(true);
    try {
      await dismissWatch(w.stashdb_id);
      onChanged();
    } catch {
      setBusy(false);
    }
  };

  const avail = w.status === "available";
  return (
    <li className={"watch-card" + (avail ? " is-available" : "")}>
      <div
        className="watch-thumb"
        role="button"
        tabIndex={0}
        onClick={() => onPickScene(w.stashdb_id, w.performer_name)}
      >
        {w.image_url ? (
          <img
            src={w.image_url}
            alt=""
            loading="lazy"
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.visibility = "hidden";
            }}
          />
        ) : null}
      </div>
      <div className="watch-main">
        <div className="watch-title">{w.title || "(untitled)"}</div>
        <div className="watch-meta">
          {w.date && <span>{w.date}</span>}
          {w.date && w.studio_name && <span className="sep">·</span>}
          {w.studio_name && <span>{w.studio_name}</span>}
          <span className="sep">·</span>
          <span className="watch-target">
            target:{" "}
            {w.target === "any"
              ? "any quality"
              : w.target === "480p"
                ? "SD"
                : w.target === "4k"
                  ? "4K"
                  : w.target}
          </span>
        </div>
        {avail && w.found_title && (
          <div className="watch-found">
            <code>{w.found_title}</code>
            <span className="watch-found-meta">
              {w.found_indexer} · {w.found_protocol} ·{" "}
              {humanSize(w.found_size || 0, "?")}
            </span>
          </div>
        )}
      </div>
      <div className="watch-actions">
        {avail ? (
          <>
            <button
              className="watch-grab"
              disabled={busy || grabbed}
              onClick={grab}
            >
              {grabbed ? "Queued ✓" : busy ? "Grabbing…" : "Grab ↓"}
            </button>
            {!grabbed && (
              <button
                className="watch-dismiss"
                disabled={busy}
                onClick={dismiss}
                title="Not this one — ignore this release and keep watching for a better one"
              >
                Dismiss
              </button>
            )}
          </>
        ) : (
          <span className="watch-spinner-label">
            <span className="coll-spinner" /> watching
          </span>
        )}
        <button className="watch-remove" disabled={busy} onClick={remove}>
          ✕
        </button>
      </div>
    </li>
  );
}
