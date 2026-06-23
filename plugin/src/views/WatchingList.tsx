import { useEffect, useRef, useState } from "react";
import {
  clearWatchBatch,
  deleteWatch,
  dismissWatch,
  fetchWatches,
  grabWatch,
  grabWatchCandidate,
  searchWatches,
  type SceneRelease,
  type Watch,
} from "../api";
import { ResBadge } from "../ResBadge";
import { humanSize } from "../format";

// Poll cadence: the watch loop runs server-side on a 30m cadence, so the
// list changes slowly — a relaxed poll keeps the "available" badge fresh
// without hammering. While a manual "search now" is running (any card
// flagged searching), poll fast so progress shows live.
const POLL_MS = 30000;
const FAST_POLL_MS = 2500;

// relKey is a stable per-candidate identity for React keys (download_url can
// be empty for magnet-less indexers).
function relKey(r: SceneRelease): string {
  return r.download_url || r.info_url || r.indexer + "|" + r.title;
}

interface Group {
  id: string; // batch_id; "" = ungrouped single tracks
  label: string;
  items: Watch[];
}

// groupWatches buckets watches by batch_id. Within a group: available first,
// then watching, then grabbed (so the actionable items are at the top and
// finished ones sink). Groups are ordered real-batches-first (most recently
// created first), with the ungrouped "Single tracks" bucket last.
function groupWatches(watches: Watch[]): Group[] {
  const order = (w: Watch) =>
    w.status === "available" ? 0 : w.status === "watching" ? 1 : 2;
  const byBatch = new Map<string, Watch[]>();
  for (const w of watches) {
    const id = w.batch_id || "";
    const arr = byBatch.get(id);
    if (arr) arr.push(w);
    else byBatch.set(id, [w]);
  }
  const groups: Group[] = [];
  for (const [id, items] of byBatch) {
    items.sort((a, b) => order(a) - order(b) || b.created_at - a.created_at);
    const label =
      id === ""
        ? "Single tracks"
        : items[0]?.batch_label || items[0]?.performer_name || "Batch";
    groups.push({ id, label, items });
  }
  // Real batches first (newest by their freshest item), singles last.
  groups.sort((a, b) => {
    if (a.id === "" && b.id !== "") return 1;
    if (b.id === "" && a.id !== "") return -1;
    const an = Math.max(...a.items.map((w) => w.created_at));
    const bn = Math.max(...b.items.map((w) => w.created_at));
    return bn - an;
  });
  return groups;
}

export default function WatchingList({
  onPickScene,
}: {
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [watches, setWatches] = useState<Watch[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);
  const [searchBusy, setSearchBusy] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  const load = async (): Promise<Watch[] | null> => {
    try {
      const r = await fetchWatches();
      setWatches(r.watches);
      setError(null);
      return r.watches;
    } catch (e) {
      setError((e as Error).message);
      return null;
    }
  };

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      if (cancelled) return;
      const ws = await load();
      // Poll fast while a search-now is in flight so cards update live.
      const fast = !!ws && ws.some((w) => w.searching);
      timer.current = window.setTimeout(tick, fast ? FAST_POLL_MS : POLL_MS);
    };
    void tick();
    return () => {
      cancelled = true;
      if (timer.current) clearTimeout(timer.current);
    };
  }, []);

  const flashToast = (msg: string) => {
    setToast(msg);
    window.setTimeout(() => setToast(null), 4500);
  };

  // Search every still-watching scene now (no scope), bounded server-side.
  const searchAll = async () => {
    setSearchBusy(true);
    try {
      const r = await searchWatches();
      flashToast(`Searching ${r.searching} scene${r.searching === 1 ? "" : "s"}…`);
      await load(); // pick up the searching flags + kick the fast poll
    } catch (e) {
      flashToast((e as Error).message || "Search failed");
    } finally {
      setSearchBusy(false);
    }
  };

  if (error && !watches)
    return <div className="empty error">Failed to load watches: {error}</div>;
  if (!watches) return <div className="empty">Loading…</div>;
  if (watches.length === 0)
    return (
      <div className="empty">
        Not watching any scenes. Hit <b>Track ▾</b> on a scene card to be told
        when a release at your chosen quality shows up, or <b>Watch all
        missing</b> on a performer to collect them as a batch — the server
        checks in the background and you grab them here. Nothing is grabbed
        automatically.
      </div>
    );

  const groups = groupWatches(watches);
  const totalAvailable = watches.filter((w) => w.status === "available").length;
  const totalWatching = watches.filter((w) => w.status === "watching").length;
  const totalSearching = watches.filter((w) => w.searching).length;

  return (
    <div>
      <div className="page-header page-header-row">
        <div>
          <h2>Watching</h2>
          <div className="meta">
            {totalAvailable > 0 && (
              <strong>{totalAvailable} ready to grab</strong>
            )}
            {totalAvailable > 0 && totalWatching > 0 && " · "}
            {totalWatching > 0 && `${totalWatching} watching`}
          </div>
        </div>
        {totalWatching > 0 && (
          <button
            className="watch-clear watch-search-now"
            disabled={searchBusy || totalSearching > 0}
            onClick={searchAll}
            title="Search every watching scene for releases now, instead of waiting for the background loop"
          >
            {totalSearching > 0
              ? `Searching ${totalSearching}…`
              : searchBusy
                ? "Searching…"
                : `Search all ${totalWatching} ↻`}
          </button>
        )}
      </div>

      {toast && <div className="ms-toast">{toast}</div>}

      {groups.map((g) => (
        <WatchGroup
          key={g.id || "__singles"}
          group={g}
          onChanged={load}
          onToast={flashToast}
          onPickScene={onPickScene}
        />
      ))}
    </div>
  );
}

function WatchGroup({
  group,
  onChanged,
  onToast,
  onPickScene,
}: {
  group: Group;
  onChanged: () => void;
  onToast: (msg: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [grabAllBusy, setGrabAllBusy] = useState(false);
  const [clearBusy, setClearBusy] = useState(false);

  const items = group.items;
  const available = items.filter((w) => w.status === "available");
  const watching = items.filter((w) => w.status === "watching");
  const grabbed = items.filter((w) => w.status === "grabbed");
  const searchingCount = items.filter((w) => w.searching).length;
  const isBatch = group.id !== "";

  const grabAll = async () => {
    setGrabAllBusy(true);
    const ids = available.map((w) => w.stashdb_id);
    const results = await Promise.allSettled(ids.map((id) => grabWatch(id)));
    const failed = results.filter((r) => r.status === "rejected").length;
    setGrabAllBusy(false);
    onToast(
      failed === 0
        ? `Queued ${ids.length} grab${ids.length === 1 ? "" : "s"} ✓`
        : `Queued ${ids.length - failed}, ${failed} failed — they stay listed for a retry`,
    );
    onChanged();
  };

  const clear = async () => {
    setClearBusy(true);
    try {
      await clearWatchBatch(group.id);
      onChanged();
    } catch {
      setClearBusy(false);
    }
  };

  // Progress line for a batch: collapses status counts into "N of M grabbed".
  const searchingNote = searchingCount > 0 ? `${searchingCount} searching` : "";
  const progress = isBatch
    ? [
        `${grabbed.length} of ${items.length} grabbed`,
        available.length > 0 ? `${available.length} ready` : "",
        watching.length > 0 ? `${watching.length} watching` : "",
        searchingNote,
      ]
        .filter(Boolean)
        .join(" · ")
    : [
        `${available.length} ready`,
        `${watching.length} watching`,
        grabbed.length ? `${grabbed.length} grabbed` : "",
        searchingNote,
      ]
        .filter(Boolean)
        .join(" · ");

  return (
    <section className="watch-group">
      <div className="watch-group-head">
        <div className="watch-group-title">
          <h3 className="section-header">{group.label}</h3>
          <span className="watch-group-progress">{progress}</span>
        </div>
        <div className="watch-group-actions">
          {available.length > 0 && (
            <button
              className="collection-cta"
              disabled={grabAllBusy}
              onClick={grabAll}
              title="Queue every available release in this group"
            >
              {grabAllBusy ? "Grabbing…" : `Grab all ${available.length} →`}
            </button>
          )}
          {isBatch && (
            <button
              className="watch-clear"
              disabled={clearBusy}
              onClick={clear}
              title="Remove every watch in this batch"
            >
              {clearBusy ? "Clearing…" : "Clear batch"}
            </button>
          )}
        </div>
      </div>
      <ul className="watch-list">
        {items.map((w) => (
          <WatchCard
            key={w.stashdb_id}
            w={w}
            onChanged={onChanged}
            onPickScene={onPickScene}
          />
        ))}
      </ul>
    </section>
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
  const [queued, setQueued] = useState(false);
  const [expanded, setExpanded] = useState(false);
  // The release the user intends to grab — defaults to the auto-picked best
  // (found_url). Choosing a different candidate re-picks via the candidate
  // endpoint instead of the plain best-grab.
  const [picked, setPicked] = useState(w.found_url || "");

  const cands = w.candidates || [];
  const canExpand = cands.length > 0;

  const grab = async () => {
    setBusy(true);
    try {
      if (picked && picked !== w.found_url) {
        await grabWatchCandidate(w.stashdb_id, picked);
      } else {
        await grabWatch(w.stashdb_id);
      }
      setQueued(true);
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
  const isGrabbed = w.status === "grabbed";
  return (
    <li
      className={
        "watch-card" +
        (avail ? " is-available" : "") +
        (isGrabbed ? " is-grabbed" : "")
      }
    >
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
        </div>
        {(avail || isGrabbed) && w.found_title && (
          <div className="watch-found">
            <code>{w.found_title}</code>
            <span className="watch-found-meta">
              {w.found_indexer} · {w.found_protocol} ·{" "}
              {humanSize(w.found_size || 0, "?")}
            </span>
            {avail && canExpand && (
              <button
                className="coll-expand watch-cand-toggle"
                onClick={() => setExpanded((e) => !e)}
                title="Show all releases this watch found — pick a different one"
              >
                {expanded ? "▾" : "▸"}{" "}
                <span className="coll-expand-n">{cands.length}</span>
              </button>
            )}
          </div>
        )}
        {avail && expanded && canExpand && (
          <div className="coll-cands">
            {cands.map((rel) => (
              <label
                key={relKey(rel)}
                className={
                  "coll-cand" + (rel.download_url === picked ? " sel" : "")
                }
              >
                <input
                  type="radio"
                  name={"pick-" + w.stashdb_id}
                  checked={rel.download_url === picked}
                  disabled={rel.download_url === ""}
                  title={
                    rel.download_url === ""
                      ? "indexer provided no download link"
                      : undefined
                  }
                  onChange={() => setPicked(rel.download_url)}
                />
                <span
                  className={
                    "coll-cand-tag " + (rel.verified ? "verified" : "guess")
                  }
                >
                  {rel.verified ? "verified" : "unverified"}{" "}
                  {rel.confidence.toFixed(2)}
                </span>
                <ResBadge title={rel.title} />
                <span className="coll-cand-body">
                  <code className="coll-cand-file">{rel.title}</code>
                  <span className="coll-cand-meta">
                    {rel.indexer} · {rel.protocol} · {humanSize(rel.size, "?")} ·{" "}
                    {rel.protocol === "usenet"
                      ? `${rel.grabs} grabs`
                      : `${rel.seeders} seeders`}
                  </span>
                </span>
              </label>
            ))}
          </div>
        )}
      </div>
      <div className="watch-actions">
        {isGrabbed ? (
          <span className="watch-grabbed-label">grabbed ✓</span>
        ) : avail ? (
          <>
            <button
              className="watch-grab"
              disabled={busy || queued}
              onClick={grab}
            >
              {queued ? "Queued ✓" : busy ? "Grabbing…" : "Grab ↓"}
            </button>
            {!queued && (
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
        ) : w.searching ? (
          <span className="watch-spinner-label searching">
            <span className="coll-spinner" /> searching…
          </span>
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
