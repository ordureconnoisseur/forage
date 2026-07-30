import { useEffect, useRef, useState } from "react";
import {
  clearFinishedWatches,
  clearWatchBatch,
  deleteWatch,
  dismissWatch,
  fetchWatches,
  grabWatch,
  grabWatchCandidate,
  redoWatch,
  searchWatches,
  type SceneRelease,
  type Watch,
} from "../api";
import { peek, store } from "../swr";
import { ResBadge, resolution } from "../ResBadge";
import { humanSize } from "../format";

// Poll cadence: the watch loop runs server-side on a 30m cadence, so the
// list changes slowly — a relaxed poll keeps the "available" badge fresh
// without hammering. While a manual "search now" is running (any card
// flagged searching), poll fast so progress shows live.
const WATCHES_KEY = "/watches";
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
  // A "batch" of one is noise: a lone watch from a subject page (or a
  // one-scene multi-select) reads better as a plain single track. The
  // group header appears once a second watch joins the batch.
  for (const [id, items] of [...byBatch]) {
    if (id !== "" && items.length === 1) {
      byBatch.delete(id);
      const singles = byBatch.get("");
      if (singles) singles.push(items[0]);
      else byBatch.set("", items);
    }
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
  onPickGrabs,
}: {
  onPickScene: (stashDBID: string, performerName?: string) => void;
  // Open Grabs filtered to some text — used by the finished-work
  // disclosure, since a grabbed watch's real state lives over there.
  onPickGrabs: (q: string) => void;
}) {
  // Seeded from the cache so returning to Watching paints the last known
  // list immediately; the poll below then refreshes it in place.
  const [watches, setWatches] = useState<Watch[] | null>(
    () => peek<{ watches: Watch[] }>(WATCHES_KEY)?.watches ?? null,
  );
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);
  const [searchBusy, setSearchBusy] = useState(false);
  const timer = useRef<number | undefined>(undefined);

  const load = async (): Promise<Watch[] | null> => {
    try {
      const r = await fetchWatches();
      store(WATCHES_KEY, r);
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
        Not watching any scenes. Hit <b>Watch</b> on a scene card to be told when
        a release shows up, or <b>Watch all missing</b> on a performer to collect
        them as a batch — the server checks in the background and you grab the
        best release here. Nothing is grabbed automatically.
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
          onPickGrabs={onPickGrabs}
        />
      ))}
    </div>
  );
}

// A group's body IS its ready-to-grab scenes. They are not wrapped in
// anything collapsible, because a control to hide them already exists one
// level up: collapsing the batch. A second chevron for the same intent was
// most of what made this feel like scaffolding around content.
//
// What DOES collapse is only what you have put away — still-searching and
// grabbed — and those live in one quiet bar at the foot of the group rather
// than as peers of the content above them. So the hierarchy reads:
//
//   the group, which you can collapse
//     the scenes you can act on
//     a footer of things tucked away
//
// One nesting level, and the second is visibly subordinate rather than
// visually identical to the first.
// ONE drawer, not one per status. Splitting it into "still searching" and
// "grabbed" spent two chevrons and two labels restating something the group
// header already says: "4 of 8 grabbed" tells you 4 are grabbed and 4 are
// not. The drawer only has to be a door.
//
// The cards inside carry their own status, so nothing is lost by mixing
// them, and "others" is the honest label — everything in this group other
// than what you can act on right now.
function WatchDrawer({
  count,
  open,
  onToggle,
}: {
  count: number;
  open: boolean;
  onToggle: () => void;
}) {
  if (count === 0) return null;
  // No chevron. The group header directly above owns one, and stacked in the
  // same column the two read as a double chevron doing one job twice. A
  // verb-led label carries the affordance instead, and says what will
  // happen rather than leaving a glyph to imply it.
  return (
    <button
      type="button"
      className={"watch-drawer" + (open ? " is-open" : "")}
      aria-expanded={open}
      onClick={onToggle}
    >
      <span className="watch-drawer-text">
        <span className="watch-drawer-verb">{open ? "hide" : "show"}</span>
        <span className="watch-drawer-count">{count}</span>
        <span className="watch-drawer-label">others</span>
      </span>
    </button>
  );
}

function WatchGroup({
  group,
  onChanged,
  onToast,
  onPickScene,
  onPickGrabs,
}: {
  group: Group;
  onChanged: () => void;
  onToast: (msg: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
  onPickGrabs: (q: string) => void;
}) {
  const [grabAllBusy, setGrabAllBusy] = useState(false);
  const [clearBusy, setClearBusy] = useState(false);

  const items = group.items;
  const available = items.filter((w) => w.status === "available");
  const watching = items.filter((w) => w.status === "watching");
  const grabbed = items.filter((w) => w.status === "grabbed");
  const searchingCount = items.filter((w) => w.searching).length;
  const isBatch = group.id !== "";
  // Finished watches are bookkeeping, not content: they exist so a batch
  // reads "9 of 30 grabbed". They were 1528 of 1913 rendered cards on the
  // reference instance (~41k DOM nodes, seconds to paint), for rows whose
  // only job is to be counted. Render the actionable ones and put the rest
  // behind a disclosure; the real state of a grabbed scene lives in Grabs.
  const [clearDoneBusy, setClearDoneBusy] = useState(false);
  // One drawer open at a time. Two open at once rebuilds the wall of cards
  // this was meant to remove, and there is no reason to compare the two.
  const [showOthers, setShowOthers] = useState(false);
  // Everything in the group other than what you can act on. Still-searching
  // first, then grabbed, which is the order groupWatches already sorts them.
  const others = items.filter((w) => w.status !== "available");
  // Nothing ready and nothing still being searched for: the batch is done.
  const settled = isBatch && available.length === 0 && watching.length === 0;

  const clearDone = async () => {
    if (clearDoneBusy) return;
    setClearDoneBusy(true);
    try {
      const r = await clearFinishedWatches(grabbed.map((w) => w.stashdb_id));
      onToast(`Cleared ${r.cleared} finished watch${r.cleared === 1 ? "" : "es"}`);
      onChanged();
    } catch (e) {
      onToast("Couldn't clear: " + (e as Error).message);
    } finally {
      setClearDoneBusy(false);
    }
  };

  // Collapse a group's card list. Default-collapse a batch that's fully done
  // (everything grabbed, nothing left to act on) so finished batches fold
  // away; otherwise start expanded. Runs once on mount — the user's later
  // toggles win.
  const [collapsed, setCollapsed] = useState(
    isBatch &&
      available.length === 0 &&
      watching.length === 0 &&
      grabbed.length > 0,
  );

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
  // The shelves below each state their own count, so the header only says
  // how far along the group is. It used to repeat every count, which is most
  // of why this looked cluttered.
  const progress = isBatch
    ? [
        `${grabbed.length} of ${items.length} grabbed`,
        searchingNote,
      ]
        .filter(Boolean)
        .join(" · ")
    : [
        `${grabbed.length} of ${items.length} grabbed`,
        searchingNote,
      ]
        .filter(Boolean)
        .join(" · ");

  return (
    <section className="watch-group">
      <div className="watch-group-head">
        <div
          className="watch-group-title"
          role="button"
          tabIndex={0}
          aria-expanded={!collapsed}
          title={collapsed ? "Expand" : "Collapse"}
          onClick={() => setCollapsed((c) => !c)}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              setCollapsed((c) => !c);
            }
          }}
        >
          <span
            className={"fchev" + (collapsed ? "" : " open")}
            aria-hidden="true"
          />
          <h3 className="section-header">{group.label}</h3>
          <span className="watch-group-progress">{progress}</span>
        </div>
      </div>
      {!collapsed && (
        <>
          {/* The body: what you can act on, rendered plainly. */}
          {available.length > 0 && (
            <ul className="watch-list">
              {available.map((w) => (
                <WatchCard
                  key={w.stashdb_id}
                  w={w}
                  onChanged={onChanged}
                  onPickScene={onPickScene}
                />
              ))}
            </ul>
          )}

          {/* The footer action bar: what is tucked away on the left, the
              primary action on the right. "Grab all" lives here rather than
              in the header because on a phone the header could not fit
              name + progress + button and wrapped it hard-left onto its own
              line, orphaned from everything. Here it is pinned to the panel
              edge, beside the content it acts on, and in thumb reach. */}
          {(others.length > 0 || available.length > 0) && (
            <div className="watch-drawers">
              <WatchDrawer
                count={others.length}
                open={showOthers}
                onToggle={() => setShowOthers((v) => !v)}
              />
              {showOthers && grabbed.length > 0 && (
                <span className="watch-drawer-actions">
                  <button
                    type="button"
                    className="setup-link"
                    onClick={() => onPickGrabs(isBatch ? group.label : "")}
                    title={
                      isBatch
                        ? "Show these in Grabs, where their download state lives"
                        : "Open Grabs, where these downloads' real state lives"
                    }
                  >
                    view in Grabs
                  </button>
                  <button
                    type="button"
                    className="setup-link watch-drawer-danger"
                    disabled={clearDoneBusy}
                    onClick={clearDone}
                    title="Remove the finished watches here. Does not touch files or grabs."
                  >
                    {clearDoneBusy ? "Clearing…" : "clear finished"}
                  </button>
                </span>
              )}
              {settled && !showOthers && (
                <span className="watch-drawer-actions">
                  <button
                    type="button"
                    className="setup-link watch-drawer-danger"
                    disabled={clearBusy}
                    onClick={clear}
                    title="Remove every watch in this finished batch"
                  >
                    clear batch
                  </button>
                </span>
              )}
              {available.length > 0 && (
                <button
                  className="collection-cta watch-grab-all"
                  disabled={grabAllBusy}
                  onClick={grabAll}
                  title="Queue every available release in this group"
                >
                  {grabAllBusy ? "Grabbing…" : `Grab all ${available.length} →`}
                </button>
              )}
            </div>
          )}
          {showOthers && (
            <ul className="watch-list watch-list-drawer">
              {others.map((w) => (
                <WatchCard
                  key={w.stashdb_id}
                  w={w}
                  onChanged={onChanged}
                  onPickScene={onPickScene}
                />
              ))}
            </ul>
          )}
        </>
      )}
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
  // WHICH action is in flight, not merely that one is. A shared boolean made
  // every button report the same thing, so dismissing a release flipped the
  // Grab button to "Grabbing…" — announcing the opposite of what was clicked.
  // Only the button you pressed changes its label; the others just disable.
  const [pending, setPending] = useState<
    null | "grab" | "dismiss" | "remove" | "redo"
  >(null);
  const busy = pending !== null;
  const [queued, setQueued] = useState(false);
  const [expanded, setExpanded] = useState(false);
  // Which release row has its score-justification ("why ranked here") panel
  // open. One at a time, keyed by row key. null = all collapsed.
  const [whyKey, setWhyKey] = useState<string | null>(null);
  // The release the user intends to grab — defaults to the auto-picked best
  // (found_url). Choosing a different candidate re-picks via the candidate
  // endpoint instead of the plain best-grab.
  const [picked, setPicked] = useState(w.found_url || "");

  // Release the in-flight state once the server's answer actually lands. The
  // handlers only clear it on failure, on the theory that success re-renders
  // the card into a different branch — but the ✕ is in every branch, so after
  // a successful dismiss it stayed disabled until a full remount. The list
  // keys cards by stashdb_id, so state survives the refetch and nothing else
  // would ever reset it.
  const seenStatus = useRef(w.status);
  useEffect(() => {
    if (seenStatus.current !== w.status) {
      seenStatus.current = w.status;
      setPending(null);
      setQueued(false);
    }
  }, [w.status]);

  const cands = w.candidates || [];
  // The count is authoritative: the list endpoint omits the blob for
  // grabbed watches, so cands.length would read 0 for them.
  const candCount = w.candidate_count ?? cands.length;
  const canExpand = candCount > 0;

  const grab = async () => {
    setPending("grab");
    try {
      if (picked && picked !== w.found_url) {
        await grabWatchCandidate(w.stashdb_id, picked);
      } else {
        await grabWatch(w.stashdb_id);
      }
      setQueued(true);
      onChanged();
    } catch {
      setPending(null);
    }
  };
  const remove = async () => {
    setPending("remove");
    try {
      await deleteWatch(w.stashdb_id);
      onChanged();
    } catch {
      setPending(null);
    }
  };
  // Reject this find AND look for another now — ignores this exact release,
  // flips back to watching, then kicks an immediate re-search rather than
  // waiting for the background loop.
  const dismiss = async () => {
    setPending("dismiss");
    try {
      await dismissWatch(w.stashdb_id);
      // Best-effort search kick. A search already in flight returns 409 —
      // fine, the watch is back to watching and the loop will pick it up.
      try {
        await searchWatches({ ids: [w.stashdb_id] });
      } catch {
        /* ignore — dismiss already succeeded */
      }
      onChanged();
    } catch {
      setPending(null);
    }
  };
  // Discard a grabbed release that turned out bad: purge the grab (download +
  // any placed file/scene), ignore that release, flip back to watching, and
  // kick an immediate re-search for a different one.
  const redo = async () => {
    setPending("redo");
    try {
      await redoWatch(w.stashdb_id);
      // Best-effort immediate re-search of just this scene. A search already
      // in flight returns 409 — fine, the watch is back to watching and the
      // loop (or next manual search) will pick it up.
      try {
        await searchWatches({ ids: [w.stashdb_id] });
      } catch {
        /* ignore — redo already succeeded */
      }
      onChanged();
    } catch {
      setPending(null);
    }
  };

  const avail = w.status === "available";
  const isGrabbed = w.status === "grabbed";
  const pickedRel = cands.find((c) => c.download_url === picked) || null;
  // found_title stands in when the blob was omitted, so the Grab button
  // keeps its resolution label.
  const pickedTitle =
    pickedRel?.title ??
    (picked && picked === w.found_url ? w.found_title : undefined);
  const pickedRes = pickedTitle ? resolution(pickedTitle) : null;
  const openScene = () => onPickScene(w.stashdb_id, w.performer_name);

  // The release rows shown under the scene. Collapsed → just the chosen one;
  // expanded → every candidate (radio-selectable). Falls back to a synthetic
  // row from the found_* fields for an older watch with no stored candidates.
  type RowData = {
    key: string;
    title: string;
    downloadUrl: string;
    indexer: string;
    protocol: string;
    size: number;
    // Optional on purpose: a row built from found_* (a grabbed watch, whose
    // candidate blob the list endpoint omits) has no seeder/grab counts.
    // Rendering 0 there states something false rather than nothing.
    grabs?: number;
    seeders?: number;
    score: number;
    scoreHits?: { label: string; points: number; reject?: boolean }[];
    reasons?: string[];
  };
  let rows: RowData[] = [];
  if (avail || isGrabbed) {
    if (cands.length > 0) {
      const src = expanded
        ? cands
        : cands.filter((c) => c.download_url === picked).slice(0, 1);
      const chosen = src.length > 0 ? src : cands.slice(0, 1);
      rows = chosen.map((r) => ({
        key: relKey(r),
        title: r.title,
        downloadUrl: r.download_url,
        indexer: r.indexer,
        protocol: r.protocol,
        size: r.size,
        grabs: r.grabs,
        seeders: r.seeders,
        score: r.score ?? 0,
        scoreHits: r.score_hits,
        reasons: r.reasons,
      }));
    } else if (w.found_title) {
      rows = [
        {
          key: "found",
          title: w.found_title,
          downloadUrl: w.found_url || "",
          indexer: w.found_indexer || "",
          protocol: w.found_protocol || "",
          size: w.found_size || 0,
          score: 0,
        },
      ];
    }
  }
  const selectable = avail && expanded && cands.length > 0;
  const others = candCount - 1;

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
        onClick={openScene}
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

      <div className="watch-body">
        <div className="watch-head">
          <div
            className="watch-headinfo"
            role="button"
            tabIndex={0}
            onClick={openScene}
          >
            <div className="watch-title">{w.title || "(untitled)"}</div>
            <div className="watch-meta">
              {w.date && <span>{w.date}</span>}
              {w.date && w.studio_name && <span className="sep">·</span>}
              {w.studio_name && <span>{w.studio_name}</span>}
            </div>
          </div>
          <div className="watch-actions">
            {isGrabbed ? (
              <>
                <span className="watch-grabbed-label">grabbed ✓</span>
                <button
                  className="watch-dismiss"
                  disabled={busy}
                  onClick={redo}
                  title="This grab was bad — remove it and search for a different release"
                >
                  {pending === "redo" ? "Searching…" : "Find another"}
                </button>
              </>
            ) : avail ? (
              <>
                <button
                  className="watch-grab"
                  disabled={busy || queued}
                  onClick={grab}
                >
                  {queued
                    ? "Queued ✓"
                    : pending === "grab"
                      ? "Grabbing…"
                      : `Grab${pickedRes ? " " + pickedRes.label : ""} ↓`}
                </button>
                {!queued && (
                  <button
                    className="watch-dismiss"
                    disabled={busy}
                    onClick={dismiss}
                    title="Ignore this release and search for a different one now"
                  >
                    {pending === "dismiss" ? "Searching…" : "Not this one"}
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
                {(w.search_count ?? 0) > 0 && (
                  <span
                    className="watch-search-count"
                    title={`Searched ${w.search_count} time${w.search_count === 1 ? "" : "s"} with no release found yet`}
                  >
                    {" · "}
                    {w.search_count}× searched
                  </span>
                )}
              </span>
            )}
            <button
              className={
                "watch-remove" + (pending === "remove" ? " is-working" : "")
              }
              disabled={busy}
              onClick={remove}
              title="Stop watching this scene"
              aria-label="Stop watching"
            >
              {pending === "remove" ? (
                <span className="coll-spinner" aria-hidden="true" />
              ) : (
                "✕"
              )}
            </button>
          </div>
        </div>

        {rows.length > 0 && (
          <div className="watch-rel">
            {rows.map((r) => {
              const sel = r.downloadUrl === picked;
              const noLink = r.downloadUrl === "";
              // The justification: the per-rule preference breakdown that
              // produced this release's score, plus the matcher's component
              // reasons. Only offer the toggle when there's something to show.
              const hits = r.scoreHits || [];
              const reasons = r.reasons || [];
              const hasWhy = hits.length > 0 || reasons.length > 0;
              const whyOpen = whyKey === r.key;
              return (
                <div key={r.key} className="watch-rel-item">
                  <label
                    className={"watch-rel-row" + (sel ? " is-selected" : "")}
                  >
                    {selectable ? (
                      <input
                        type="radio"
                        name={"pick-" + w.stashdb_id}
                        checked={sel}
                        disabled={noLink}
                        title={
                          noLink
                            ? "indexer provided no download link"
                            : undefined
                        }
                        onChange={() => setPicked(r.downloadUrl)}
                      />
                    ) : (
                      <span className="watch-rel-dot" aria-hidden="true" />
                    )}
                    <span className="watch-rel-q">
                      <ResBadge title={r.title} />
                    </span>
                    <code className="watch-rel-file">{r.title}</code>
                    <span className="watch-rel-meta">
                      {[
                        humanSize(r.size, "?"),
                        r.indexer,
                        r.protocol === "usenet"
                          ? r.grabs != null && `${r.grabs} grabs`
                          : r.seeders != null && `${r.seeders} seeders`,
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </span>
                    {hasWhy && (
                      <button
                        type="button"
                        className={
                          "watch-rel-why" + (whyOpen ? " open" : "")
                        }
                        aria-expanded={whyOpen}
                        title="Why this release ranks here"
                        onClick={(e) => {
                          // Inside a <label>: stop the click from toggling
                          // the row's radio selection.
                          e.preventDefault();
                          e.stopPropagation();
                          setWhyKey(whyOpen ? null : r.key);
                        }}
                      >
                        <span className="watch-rel-score">
                          {r.score >= 0 ? "+" : ""}
                          {r.score}
                        </span>
                        <span
                          className={"fchev" + (whyOpen ? " open" : "")}
                          aria-hidden="true"
                        />
                      </button>
                    )}
                  </label>
                  {whyOpen && (
                    <div className="watch-rel-why-panel">
                      {hits.length > 0 && (
                        <ul className="watch-rel-why-hits">
                          {hits.map((h, i) => (
                            <li
                              key={i}
                              className={h.reject ? "is-reject" : ""}
                            >
                              <span className="why-label">{h.label}</span>
                              <span className="why-pts">
                                {h.points >= 0 ? "+" : ""}
                                {h.points}
                              </span>
                            </li>
                          ))}
                        </ul>
                      )}
                      {reasons.length > 0 && (
                        <div className="watch-rel-why-match">
                          <span className="why-cap">match</span>{" "}
                          {reasons.join(" · ")}
                        </div>
                      )}
                    </div>
                  )}
                </div>
              );
            })}
            {avail && canExpand && others > 0 && (
              <button
                className="watch-rel-more"
                onClick={() => setExpanded((e) => !e)}
                title="Show every release this watch found — pick a different one"
              >
                <span
                  className={"fchev" + (expanded ? " open" : "")}
                  aria-hidden="true"
                />
                {expanded
                  ? "Show fewer"
                  : `${others} other release${others === 1 ? "" : "s"}`}
              </button>
            )}
          </div>
        )}
      </div>
    </li>
  );
}
