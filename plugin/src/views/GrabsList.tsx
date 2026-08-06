import {
  Fragment,
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { humanSize, isUnfiledFolder } from "../format";
import { peek, store } from "../swr";
import {
  ACTIVE_STATUSES,
  adoptDownloads,
  retryFailedGrabs,
  deleteGrab,
  DeleteGrabResult,
  fetchGrabDetail,
  fetchGrabs,
  fetchGrabDeletePreview,
  type GrabDeletePreview,
  Grab,
  GrabDetail,
  grabTorrentFile,
  inspectTorrentFile,
  type TorrentInspect,
  matchGrab,
  type MatchCandidate,
  fetchGrabMatchCandidates,
  retryGrab,
  setGrabPerformer,
  fetchPerformers,
  refreshPerformers,
  resolveDuplicate,
  type DuplicateReview,
  type SceneCopy,
  fetchPackScenes,
  applyPackPerformer,
  type PackScenes,
  GrabStatus,
  type GrabFilter,
  isActiveStatus,
  proxiedImageURL,
  performerImageURL,
  fetchSceneCard,
  type SceneCard,
} from "../api";
import { createPortal } from "react-dom";

// GrabsList surfaces the full state machine the poller advances:
//
//   queued → downloading → completed → placed → scanned → confirmed
//                                                       ↘ mismatched
//                                                       ↘ orphaned
//                                ↘ failed
//
// Auto-polls /grabs while the tab is visible; fast cadence when any
// non-terminal rows exist, slow when everything has settled. This is
// the "what's happening to my downloads" view — it answers questions
// the SceneReleases grab button can't (since that button stops at
// "Queued → qbit" right after submit and never updates).

const FAST_POLL_MS = 5_000;
const SLOW_POLL_MS = 30_000;
// How many grabs one page fetches. The live view shows the newest page and
// polls it; searching/filtering re-queries the whole table server-side, and
// "Load more" appends further pages.
const PAGE_SIZE = 100;

// Cache key for the FIRST page of a given filter+query. Only the un-paged
// view is ever cached: once the user pages, the accumulated list is a
// different thing and restoring it would fight the offset bookkeeping.
function grabsKey(filter: string, q: string): string {
  return "/grabs?status=" + filter + "&q=" + q;
}

// What the first page needs to paint: the rows plus the counts the filter
// pills and "showing N of M" read, so a restore is self-consistent.
interface GrabsPage {
  grabs: Grab[];
  totals: Partial<Record<GrabFilter, number>>;
  matchTotal?: number;
}

// The filter pills mirror the poller's state machine in two visual
// groups: the in-flight pipeline (linear progression, shown with
// arrows) and the terminal outcomes (confirmed is the happy end;
// mismatched/orphaned/failed are the off-ramps). "All" is a master
// toggle that leads the strip.
const IN_FLIGHT: GrabStatus[] = [
  "queued",
  "downloading",
  "completed",
  "placed",
  "scanned",
];
// retryEta renders when a deferred grab's next automatic attempt fires,
// from the daemon-provided unix time. Coarse on purpose: backoffs are
// minutes to an hour and the row isn't fast-polled while deferred.
function retryEta(at?: number): string {
  if (!at) return "";
  const secs = at - Math.floor(Date.now() / 1000);
  // A slot more than a couple of minutes past due means the daemon is
  // deliberately holding the retry (download client outage; see the red
  // banner) rather than about to run it: promise nothing.
  if (secs <= -120) return "· waiting to retry";
  if (secs <= 30) return "· retrying shortly";
  if (secs < 90) return "· retries in ~1m";
  if (secs < 3600) return `· retries in ~${Math.round(secs / 60)}m`;
  return `· retries in ~${Math.round(secs / 3600)}h`;
}

// "unmatched" rides here as a pseudo-status resolved server-side: it splits
// the confirmed rows into the ones Stash actually linked to a StashDB scene
// and the ones that merely landed. Sitting next to confirmed makes the pair
// read as the two ways a grab can end up in the library, and gives the amber
// NO MATCH rows a way to be listed on their own.
const OUTCOME: GrabFilter[] = [
  "confirmed",
  "unmatched",
  "mismatched",
  "orphaned",
  "deferred",
  "failed",
];

// A grab reaches the terminal "confirmed" status two very different ways:
// Stash's phash linked the file to a StashDB scene (a real match, actual id
// set), or the file merely landed in the library and no cross-id ever
// arrived — the daemon settles those as confirmed too, with reason
// "in library (scanned)" / "in library; no StashDB match". Both are
// status=confirmed, so without this the two render as the same green chip
// and you can't tell a matched scene from an unidentified one.
//
// Packs are excluded: a pack is many scenes and never carries a single
// actual id — its identify progress is the n/m counter on the row.
//
// Note this reads the GRAB's record, which is what the list has. If you
// identify the file by hand in Stash afterwards the grab is already
// terminal and never learns about it — the expanded card's IdentifyBlock
// checks the live Stash cross-id and will say "Identified in Stash".
function isUnmatched(g: Grab): boolean {
  return g.status === "confirmed" && g.kind !== "pack" && !g.actual_stashdb_id;
}

// PendingTorrent is one queued .torrent: its own inspection, its own
// destination folder, and its own outcome. Per-row state is what lets one bad
// torrent fail without taking the rest of a dropped batch with it.
interface PendingTorrent {
  key: number;
  file: File;
  inspect: TorrentInspect | null;
  name: string;
  reading: boolean;
  adding: boolean;
  done?: number;
  err?: string | null;
}

export default function GrabsList({
  onPickScene,
  initialQ,
}: {
  // Jump to a scene's releases view (to pick a different release when a
  // grab stalled or failed). Receives the scene's StashDB id + the
  // performer to place under.
  onPickScene: (stashDBID: string, performerName?: string) => void;
  // Starting search text, from the ?q= route param, so another view can
  // link into a filtered Grabs.
  initialQ?: string;
}) {
  // Seeded from the first-page cache so returning to Grabs paints the last
  // known page immediately; the poll below revalidates within a tick.
  const seed = initialQ ? null : peek<GrabsPage>(grabsKey("any", ""));
  const [grabs, setGrabs] = useState<Grab[]>(() => seed?.grabs ?? []);
  const [totals, setTotals] = useState<Partial<Record<GrabFilter, number>>>(
    () => seed?.totals ?? {},
  );
  // Total grabs matching the current status+q filter across the whole table
  // (from the daemon). Drives the result count and "load more" end-detection;
  // undefined until the first response (or on an older daemon that omits it).
  const [matchTotal, setMatchTotal] = useState<number | undefined>(undefined);
  const [loading, setLoading] = useState(() => seed === null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<GrabFilter>("any");
  const [q, setQ] = useState(initialQ ?? "");
  // Debounced copy of q — the server query runs off this so each keystroke
  // doesn't fire a request.
  const [qDebounced, setQDebounced] = useState(initialQ ?? "");
  const [expanded, setExpanded] = useState<Set<number>>(new Set());
  const [notice, setNotice] = useState<string | null>(null);
  const [adopting, setAdopting] = useState(false);
  const [retryingAll, setRetryingAll] = useState(false);
  const [addOpen, setAddOpen] = useState(false);
  // The add form holds a QUEUE, not one file. Dropping a folder's worth of
  // .torrents is the normal case for a private tracker, and making that five
  // round trips through a picker is the thing drag-and-drop was supposed to
  // fix. Each entry carries its own inspection and its own destination folder,
  // because five torrents rarely belong to one performer.
  const [queue, setQueue] = useState<PendingTorrent[]>([]);
  const [addBusy, setAddBusy] = useState(false);
  const [addErr, setAddErr] = useState<string | null>(null);
  const queueKey = useRef(0);
  const fileRef = useRef<HTMLInputElement>(null);
  const lastFetch = useRef(0);
  // Whether the LAST SUCCESSFUL poll saw active grabs — drives the
  // fast/slow cadence and survives a transient fetch failure.
  const lastActive = useRef(false);
  // How many rows are currently loaded (page 0 + any "load more" pages), so
  // a refetch after a mutation re-pulls the same span rather than snapping
  // back to one page.
  const loadedRef = useRef(0);
  // Set once the user pages past the first page: freezes the live poll so a
  // tick can't wipe the appended history out from under them. Reset whenever
  // the filter/query changes (the effect re-runs from a clean page 0).
  const pagedRef = useRef(false);

  // Immediate refetch of the current view (same filter + query), used after a
  // mutation so the row updates without waiting for the next poll tick.
  const refresh = useCallback(async () => {
    try {
      const limit = Math.min(500, Math.max(PAGE_SIZE, loadedRef.current));
      const r = await fetchGrabs({ status: filter, q: qDebounced, limit });
      setGrabs(r.grabs);
      setTotals(r.totals || {});
      setMatchTotal(r.match_total);
      loadedRef.current = r.grabs.length;
    } catch {
      /* next poll tick will recover */
    }
  }, [filter, qDebounced]);

  // Append the next page of matches. Freezes the live poll (pagedRef) so the
  // accumulated list survives; dedupes by id in case a poll/insert shifted
  // the offset.
  const loadMore = useCallback(async () => {
    if (loadingMore) return;
    setLoadingMore(true);
    pagedRef.current = true;
    try {
      const r = await fetchGrabs({
        status: filter,
        q: qDebounced,
        limit: PAGE_SIZE,
        offset: loadedRef.current,
      });
      setGrabs((prev) => {
        const seen = new Set(prev.map((g) => g.id));
        const merged = prev.concat(r.grabs.filter((g) => !seen.has(g.id)));
        loadedRef.current = merged.length;
        return merged;
      });
      setMatchTotal(r.match_total);
    } catch (e) {
      setNotice("Load more failed: " + (e as Error).message);
    } finally {
      setLoadingMore(false);
    }
  }, [filter, qDebounced, loadingMore]);

  // Force-adopt torrents added to the download client manually, now —
  // bypassing the 5-minute grace, so a bulk manual add shows up immediately
  // instead of after the wait.
  async function scanForDownloads() {
    if (adopting) return;
    setAdopting(true);
    try {
      const res = await adoptDownloads();
      setNotice(
        res.adopted > 0
          ? `Adopted ${res.adopted} download${res.adopted === 1 ? "" : "s"}`
          : res.skippedRecent > 0
            ? `${res.skippedRecent} too new, auto-adopting soon`
            : "Nothing new to adopt",
      );
      await refresh();
    } catch (e) {
      setNotice("Adopt failed: " + (e as Error).message);
    } finally {
      setAdopting(false);
    }
  }

  // Re-queue every failed grab at once — bulk recovery after a batch where
  // many failed.
  async function retryAllFailed() {
    if (retryingAll) return;
    setRetryingAll(true);
    try {
      const res = await retryFailedGrabs();
      setNotice(
        `Retrying ${res.retried} failed grab${res.retried === 1 ? "" : "s"}` +
          (res.skipped ? ` (${res.skipped} skipped — no URL)` : ""),
      );
      await refresh();
    } catch (e) {
      setNotice("Retry failed: " + (e as Error).message);
    } finally {
      setRetryingAll(false);
    }
  }

  // Clear the add form back to empty — deselects the file, drops the
  // inspect result, and clears the folder. (Does not close the form.)
  const resetAddForm = useCallback(() => {
    setQueue([]);
    setAddErr(null);
    if (fileRef.current) fileRef.current.value = "";
  }, []);

  // Picking a file inspects it (parse name/size/counts + suggest a
  // performer folder) before any download — confirm-first.
  // Taking .torrents, from wherever they came: the picker or a drop. Both
  // paths land here so inspection, pack detection and folder suggestions are
  // identical however the files arrived.
  //
  // Inspections run concurrently and each writes only its OWN row, so a slow
  // or corrupt torrent never holds up the rest of the batch.
  const acceptTorrents = useCallback((files: File[]) => {
    const torrents = files.filter((f) =>
      f.name.toLowerCase().endsWith(".torrent"),
    );
    if (torrents.length === 0) {
      setAddErr(
        files.length === 1
          ? `${files[0].name} isn't a .torrent file`
          : "none of those are .torrent files",
      );
      return;
    }
    setAddErr(
      torrents.length < files.length
        ? `ignored ${files.length - torrents.length} file(s) that aren't .torrent`
        : null,
    );
    const added: PendingTorrent[] = torrents.map((file) => ({
      key: (queueKey.current += 1),
      file,
      inspect: null,
      name: "",
      reading: true,
      adding: false,
    }));
    setQueue((q) => [...q, ...added]);
    for (const item of added) {
      void inspectTorrentFile(item.file)
        .then((ins) =>
          setQueue((q) =>
            q.map((it) =>
              it.key === item.key
                ? {
                    ...it,
                    reading: false,
                    inspect: ins,
                    // Pre-fill only when the name has not been touched, so a
                    // late inspection cannot overwrite a choice already made.
                    name: it.name || (ins.suggested_performers[0]?.name ?? ""),
                  }
                : it,
            ),
          ),
        )
        .catch((e: Error) =>
          setQueue((q) =>
            q.map((it) =>
              it.key === item.key
                ? { ...it, reading: false, err: e.message }
                : it,
            ),
          ),
        );
    }
  }, []);

  const [dropping, setDropping] = useState(false);
  const dragDepth = useRef(0);

  const onPickTorrent = useCallback(() => {
    acceptTorrents(Array.from(fileRef.current?.files ?? []));
    // Clear the input so picking the SAME file again still fires onChange.
    if (fileRef.current) fileRef.current.value = "";
  }, [acceptTorrents]);

  // Drag a .torrent anywhere onto the page.
  //
  // Window-level rather than a bordered drop zone, because a target you have
  // to aim at is barely better than the file picker it replaces: the point is
  // to drop the file the moment you have it, not to go and find where it goes.
  // Dropping also OPENS the add form, so one gesture replaces click-add,
  // click-browse, find-the-file, choose.
  //
  // preventDefault on dragover is what makes a drop possible at all, and
  // without it the browser navigates away to render the .torrent instead.
  useEffect(() => {
    const hasFiles = (e: DragEvent) =>
      Array.from(e.dataTransfer?.items ?? []).some((i) => i.kind === "file");
    const onEnter = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      dragDepth.current += 1;
      setDropping(true);
    };
    const onOver = (e: DragEvent) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
    };
    // dragleave fires for every nested element the cursor crosses, so a depth
    // counter is what keeps the overlay from flickering off mid-drag.
    const onLeave = () => {
      dragDepth.current = Math.max(0, dragDepth.current - 1);
      if (dragDepth.current === 0) setDropping(false);
    };
    const onDropped = (e: DragEvent) => {
      const files = Array.from(e.dataTransfer?.files ?? []);
      if (files.length === 0) return;
      e.preventDefault();
      dragDepth.current = 0;
      setDropping(false);
      setAddOpen(true);
      acceptTorrents(files);
    };
    window.addEventListener("dragenter", onEnter);
    window.addEventListener("dragover", onOver);
    window.addEventListener("dragleave", onLeave);
    window.addEventListener("drop", onDropped);
    return () => {
      window.removeEventListener("dragenter", onEnter);
      window.removeEventListener("dragover", onOver);
      window.removeEventListener("dragleave", onLeave);
      window.removeEventListener("drop", onDropped);
    };
  }, [acceptTorrents]);

  // Add every queued torrent, sequentially.
  //
  // Sequential rather than parallel because each add is a write to qBit and a
  // row in forage's grabs table; ten at once buys nothing and makes a partial
  // failure much harder to describe. Each row records its own outcome, so one
  // rejected torrent leaves the other nine added and visibly says which failed
  // rather than collapsing the batch into a single error.
  const submitQueue = useCallback(async () => {
    const pending = queue.filter((it) => it.done === undefined && !it.reading);
    if (pending.length === 0) {
      setAddErr("nothing to add yet");
      return;
    }
    setAddBusy(true);
    setAddErr(null);
    let ok = 0;
    for (const item of pending) {
      setQueue((q) =>
        q.map((it) =>
          it.key === item.key ? { ...it, adding: true, err: null } : it,
        ),
      );
      try {
        const res = await grabTorrentFile(item.file, item.name.trim());
        ok += 1;
        setQueue((q) =>
          q.map((it) =>
            it.key === item.key
              ? { ...it, adding: false, done: res.grab_id }
              : it,
          ),
        );
      } catch (e) {
        setQueue((q) =>
          q.map((it) =>
            it.key === item.key
              ? { ...it, adding: false, err: (e as Error).message }
              : it,
          ),
        );
      }
    }
    setAddBusy(false);
    if (ok > 0) {
      setNotice(`Added ${ok} torrent${ok === 1 ? "" : "s"}`);
      void refresh();
    }
    // Only close when everything landed; a failed row has to stay visible.
    setQueue((q) => {
      const left = q.filter((it) => it.done === undefined);
      if (left.length === 0) {
        setAddOpen(false);
        if (fileRef.current) fileRef.current.value = "";
        return [];
      }
      return q;
    });
  }, [queue, refresh]);

  const setItemName = useCallback((key: number, name: string) => {
    setQueue((q) => q.map((it) => (it.key === key ? { ...it, name } : it)));
  }, []);

  const dropItem = useCallback((key: number) => {
    setQueue((q) => q.filter((it) => it.key !== key));
  }, []);

  const handleDeleted = useCallback(
    (id: number, res: DeleteGrabResult) => {
      setExpanded((s) => {
        const next = new Set(s);
        next.delete(id);
        return next;
      });
      if (res.errors && res.errors.length > 0) {
        setNotice(`Removed with issues: ${res.errors.join("; ")}`);
      } else {
        setNotice(`Removed: ${res.removed.join(", ")}`);
      }
      void refresh();
    },
    [refresh],
  );

  // Debounce the search box → qDebounced, which the server query runs off, so
  // each keystroke doesn't fire a request.
  useEffect(() => {
    const t = window.setTimeout(() => setQDebounced(q.trim()), 300);
    return () => window.clearTimeout(t);
  }, [q]);

  // Load + (in the default view only) live-poll the grabs list. Re-runs
  // whenever the status filter or debounced search changes, resetting to a
  // clean page 0. The fast/slow cadence for in-flight download progress runs
  // only for the unfiltered default view; once you filter, search, or page
  // past the first page (pagedRef), the view is a static server-side query so
  // a poll tick can't clobber what you're reading. Uses setTimeout (not
  // setInterval) so the delay adapts to whether anything is active.
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    let inFlight = false;
    const live = filter === "any" && qDebounced === "";
    pagedRef.current = false;
    setLoading(true);

    async function tick() {
      if (cancelled || inFlight) return;
      if (document.hidden) {
        // Don't waste cycles when the tab isn't visible. Reschedule
        // and try again later — visibilitychange handler also kicks.
        timer = window.setTimeout(tick, SLOW_POLL_MS);
        return;
      }
      // On a FAILED fetch, reuse the last successful answer: one transient
      // blip mid-download shouldn't drop an active grab to the slow cadence.
      inFlight = true;
      try {
        const r = await fetchGrabs({
          status: filter,
          q: qDebounced,
          limit: PAGE_SIZE,
        });
        if (cancelled) return;
        setGrabs(r.grabs);
        setTotals(r.totals || {});
        setMatchTotal(r.match_total);
        loadedRef.current = r.grabs.length;
        // Only the un-paged page-1 answer is cacheable (see grabsKey).
        if (!pagedRef.current) {
          store<GrabsPage>(grabsKey(filter, qDebounced), {
            grabs: r.grabs,
            totals: r.totals || {},
            matchTotal: r.match_total,
          });
        }
        setError(null);
        lastFetch.current = Date.now();
        lastActive.current = r.grabs.some((g) => isActiveStatus(g.status));
      } catch (e) {
        if (cancelled) return;
        setError((e as Error).message);
      } finally {
        inFlight = false;
        if (!cancelled) setLoading(false);
      }
      // Only the live default view keeps polling, and only until the user
      // pages (pagedRef) — otherwise a tick would wipe the appended history.
      if (cancelled || !live || pagedRef.current) return;
      timer = window.setTimeout(tick, lastActive.current ? FAST_POLL_MS : SLOW_POLL_MS);
    }

    // A different filter/query is a different key: show its cached page if
    // we have one instead of dropping to a spinner.
    const hit = peek<GrabsPage>(grabsKey(filter, qDebounced));
    if (hit) {
      setGrabs(hit.grabs);
      setTotals(hit.totals);
      setMatchTotal(hit.matchTotal);
      loadedRef.current = hit.grabs.length;
      setLoading(false);
    }
    tick();
    const onVis = () => {
      if (
        live &&
        !pagedRef.current &&
        !document.hidden &&
        Date.now() - lastFetch.current > FAST_POLL_MS
      ) {
        // Tab refocused after a hidden period — refetch immediately.
        // The inFlight guard keeps this from FORKING the loop: while a
        // tick's fetch is awaited, `timer` still holds an already-fired
        // timeout id, so clearTimeout alone is a no-op and a second
        // tick() here would start a parallel self-rescheduling chain,
        // permanently doubling the poll rate.
        if (timer) clearTimeout(timer);
        tick();
      }
    };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVis);
    };
    // Re-runs only on filter/query change. refresh/loadMore mutate the same
    // state via their own closures and are intentionally not deps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filter, qDebounced]);

  // Group the loaded grabs (already filtered server-side) into render items: a
  // scene with 2+ attempts becomes a single SceneGroup card; everything else
  // (lone grabs, packs, and grabs with no scene id) stays a standalone row.
  // This is what makes "how many torrents am I trying for this scene" legible
  // — every retry / pick-another for one scene shares its stashdb id, so they
  // collapse into one card. Order follows each grab's position, so the
  // newest-first ordering is preserved.
  const items = useMemo<GrabListItem[]>(
    () => groupGrabsByScene(grabs),
    [grabs],
  );

  // Only block on the very first load; keep the list visible during
  // background refetches (filter changes, polls) so it doesn't flash.
  if (loading && grabs.length === 0)
    return <div className="empty">Loading grabs…</div>;
  if (error && grabs.length === 0)
    return <div className="empty error">Failed to load: {error}</div>;

  const anyTotal = Object.values(totals).reduce((s, n) => s + (n || 0), 0);
  const searching = qDebounced !== "" || filter !== "any";

  return (
    <div>
      {dropping && (
        <div className="grab-drop-overlay">
          <div className="grab-drop-card">
            <strong>Drop a .torrent</strong>
            <span>forage reads it, suggests a folder, and hands it to qBit</span>
          </div>
        </div>
      )}
      {/* Toolbar: identity (title + stats) on the left, search +
          live count on the right — one row that uses the width. */}
      <div className="grab-toolbar">
        <div className="grab-toolbar-id">
          <h2>Grabs</h2>
          <span className="grab-toolbar-stats">
            {anyTotal} total ·{" "}
            {([...ACTIVE_STATUSES, "deferred"] as GrabStatus[]).reduce(
              (s, k) => s + (totals[k] || 0),
              0,
            )}{" "}
            in flight
          </span>
        </div>
        <div className="grab-toolbar-search">
          <button
            className={"grab-add-btn" + (addOpen ? " active" : "")}
            onClick={() => {
              if (addOpen) resetAddForm();
              setAddOpen((o) => !o);
            }}
          >
            + Add torrent
          </button>
          <input
            type="text"
            placeholder="Search all grabs by title, performer, indexer…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          {/* Result count across the WHOLE table for the active filter — only
              shown when a filter/search is narrowing (a bare total is noise). */}
          {searching && matchTotal !== undefined && (
            <span className="grab-toolbar-count">
              {matchTotal} match{matchTotal === 1 ? "" : "es"}
            </span>
          )}
        </div>
      </div>

      {addOpen && (
        <div className="grab-add-form">
          <input
            ref={fileRef}
            type="file"
            accept=".torrent"
            multiple
            onChange={onPickTorrent}
          />
          {queue.length > 0 && (
            <div className="ti-queue">
              {queue.map((it) => (
                <div
                  key={it.key}
                  className={
                    "ti-row" +
                    (it.done !== undefined ? " done" : "") +
                    (it.err ? " failed" : "")
                  }
                >
                  <div className="ti-summary">
                    {it.inspect ? (
                      <span className={"ti-kind " + it.inspect.kind}>
                        {it.inspect.kind === "pack" ? "PACK" : "SINGLE"}
                      </span>
                    ) : (
                      <span className="ti-kind">…</span>
                    )}
                    <strong className="ti-name">
                      {it.inspect?.name || it.file.name}
                    </strong>
                    {it.inspect && (
                      <span className="ti-meta">
                        {it.inspect.video_count} video
                        {it.inspect.video_count === 1 ? "" : "s"} ·{" "}
                        {humanSize(it.inspect.total_size, "?")}
                      </span>
                    )}
                    {it.reading && <span className="ti-meta">reading…</span>}
                    {it.adding && <span className="ti-meta">adding…</span>}
                    {it.done !== undefined && (
                      <span className="ti-meta">→ grab #{it.done}</span>
                    )}
                    {it.done === undefined && !it.adding && (
                      <button
                        type="button"
                        className="ti-drop"
                        onClick={() => dropItem(it.key)}
                        title="Remove from the queue"
                      >
                        ✕
                      </button>
                    )}
                  </div>
                  {it.err && <div className="grab-add-err">{it.err}</div>}
                  {it.done === undefined && (
                    <div className="ti-rowfolder">
                      {(it.inspect?.suggested_performers.length ?? 0) > 0 && (
                        <div className="ti-suggest">
                          {it.inspect?.suggested_performers.map((pf) => (
                            <button
                              key={pf.stash_id}
                              type="button"
                              className={
                                "ti-chip" + (it.name === pf.name ? " sel" : "")
                              }
                              onClick={() => setItemName(it.key, pf.name)}
                              title={`${pf.scene_count} scenes in library`}
                            >
                              {pf.favorite ? "★ " : ""}
                              {pf.name}
                            </button>
                          ))}
                        </div>
                      )}
                      <input
                        type="text"
                        placeholder="folder — default (manual)"
                        value={it.name}
                        onChange={(e) => setItemName(it.key, e.target.value)}
                      />
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
          <button
            className="grab-add-go"
            onClick={submitQueue}
            disabled={
              addBusy ||
              queue.length === 0 ||
              queue.every((it) => it.done !== undefined || it.reading)
            }
          >
            {addBusy
              ? "Adding…"
              : queue.filter((it) => it.done === undefined).length > 1
                ? `Add ${queue.filter((it) => it.done === undefined).length}`
                : "Add"}
          </button>
          {queue.length > 0 && (
            <button
              type="button"
              className="grab-add-clear"
              onClick={resetAddForm}
              disabled={addBusy}
              title="Clear the queue"
            >
              ✕ Clear
            </button>
          )}
          {addErr && <span className="grab-add-err">{addErr}</span>}
          <span className="grab-add-hint">
            Drop .torrent files anywhere. forage downloads each, places into{" "}
            <code>/Media/&lt;folder&gt;</code>, scans and identifies —
            auto-detecting pack vs single.
          </span>
        </div>
      )}

      {/* Filter strip — one compact row. "all" leads, then the
          in-flight pipeline (arrow-linked), a divider, then the
          outcome states (status dots). Inline micro-labels, no
          stacked headers, everything centre-aligned. */}
      <div className="grab-filter">
        {/* On desktop this wrapper is display:contents (invisible to
            layout); on phones it becomes a horizontally swipeable strip
            so the chips never stack into a wall. */}
        <div className="grab-filter-chips">
          <button
            className={
              "grab-chip chip-any" + (filter === "any" ? " active" : "")
            }
            onClick={() => setFilter("any")}
          >
            <span className="chip-label">all</span>
            <span className="chip-count">{anyTotal}</span>
          </button>

          <span className="grab-filter-sep" />
          <span className="grab-filter-label">flight</span>
          <div className="grab-flow-pills">
            {IN_FLIGHT.map((s, i) => (
              <Fragment key={s}>
                {i > 0 && (
                  <span className="grab-flow-arrow" aria-hidden="true">
                    ›
                  </span>
                )}
                <FilterChip
                  status={s}
                  count={totals[s] || 0}
                  active={filter === s}
                  onClick={() => setFilter(s)}
                />
              </Fragment>
            ))}
          </div>

          <span className="grab-filter-sep" />
          <span className="grab-filter-label">outcome</span>
          <div className="grab-flow-pills">
            {OUTCOME.map((s) => (
              <FilterChip
                key={s}
                status={s}
                count={totals[s] || 0}
                active={filter === s}
                onClick={() => setFilter(s)}
                dot
              />
            ))}
          </div>
        </div>

        {/* List-level actions, right-aligned. */}
        <span className="grab-filter-actions">
          {(totals.failed ?? 0) > 0 && (
            <button
              className="grab-adopt-btn"
              onClick={retryAllFailed}
              disabled={retryingAll}
              title="Re-queue every failed grab that still has a download URL"
            >
              {retryingAll ? "Retrying…" : `↻ Retry ${totals.failed} failed`}
            </button>
          )}
          {/* Unfiled lives next to Deletions rather than in the main nav: it
              is a place you go on purpose, not a queue that should be shouting
              a number at you. Most of what it holds is unidentified amateur
              content that will never be identifiable, so a badge here would be
              a count that can never reach zero. */}
          <a
            className="grab-adopt-btn"
            href="#/unfiled"
            title="Library scenes that are not under a performer folder, and the ones Stash can name a performer for"
          >
            Unfiled ↗
          </a>
          <a
            className="grab-adopt-btn"
            href="#/deletions"
            title="The deletion journal — everything forage removed or refused, with restore for trashed items"
          >
            Deletions ↗
          </a>
          {/* Force-adopt torrents added to the client manually, right now. */}
          <button
            className="grab-adopt-btn"
            onClick={scanForDownloads}
            disabled={adopting}
            title="Pick up downloads you added to qBittorrent or SABnzbd yourself (forage category). qBit torrents adopt right away; SAB jobs once they finish downloading."
          >
            {adopting ? "Adopting…" : "↻ Adopt downloads"}
          </button>
        </span>
      </div>

      {notice && (
        <div className="grab-notice" onClick={() => setNotice(null)}>
          {notice}
          <span className="grab-notice-x">×</span>
        </div>
      )}

      {grabs.length === 0 ? (
        <div className="empty">
          {searching ? "No grabs match this filter." : "No grabs yet."}
        </div>
      ) : (
        <ul className="grab-list">
          {items.map((item) => {
            const rowProps = (g: Grab) => ({
              g,
              expanded: expanded.has(g.id),
              onToggle: () =>
                setExpanded((s) => {
                  const next = new Set(s);
                  if (next.has(g.id)) next.delete(g.id);
                  else next.add(g.id);
                  return next;
                }),
              onDeleted: (res: DeleteGrabResult) => handleDeleted(g.id, res),
              onMatched: () => {
                setNotice(`Matched grab #${g.id} to StashDB`);
                void refresh();
              },
              onRetried: () => {
                setNotice(`Retrying grab #${g.id}…`);
                void refresh();
              },
              onPerformerSet: (name: string) => {
                setNotice(`Filed under ${name}`);
                void refresh();
              },
              onResolvedDuplicate: (msg: string) => {
                setNotice(msg);
                void refresh();
              },
              onPickScene,
            });
            if (item.kind === "group") {
              return (
                <SceneGroup key={item.key} group={item}>
                  {item.grabs.map((g) => (
                    <GrabRow key={g.id} {...rowProps(g)} />
                  ))}
                </SceneGroup>
              );
            }
            return <GrabRow key={item.grab.id} {...rowProps(item.grab)} />;
          })}
        </ul>
      )}

      {matchTotal !== undefined && grabs.length < matchTotal && (
        <div className="grab-loadmore">
          <button
            className="grab-loadmore-btn"
            onClick={loadMore}
            disabled={loadingMore}
          >
            {loadingMore
              ? "Loading…"
              : `Load more — showing ${grabs.length} of ${matchTotal}`}
          </button>
        </div>
      )}

    </div>
  );
}

function FilterChip({
  status,
  count,
  active,
  onClick,
  dot,
}: {
  status: GrabFilter;
  count: number;
  active: boolean;
  onClick: () => void;
  dot?: boolean;
}) {
  return (
    <button
      className={
        "grab-chip chip-" +
        status +
        (active ? " active" : "") +
        // Phones hide empty chips to cut clutter; desktop keeps them so
        // the pipeline reads as a flow. An active chip stays visible
        // even at zero so the filter can be escaped.
        (count === 0 && !active ? " zero" : "")
      }
      onClick={onClick}
    >
      {dot && <span className="chip-dot" aria-hidden="true" />}
      <span className="chip-label">{status}</span>
      <span className="chip-count">{count}</span>
    </button>
  );
}

// Pipeline renders the grab's life-cycle as a horizontal stepper:
// Grabbed → Downloaded → Placed → <terminal>. Nodes light when their
// timestamp exists; connectors fill only between consecutive done
// nodes. The terminal node adapts to the grab's outcome (confirmed /
// mismatched / orphaned / failed) or shows a pending/active state
// while the file is still working through Stash.
function Pipeline({ g }: { g: Grab }) {
  type Step = {
    label: string;
    at: number;
    done: boolean;
    active?: boolean;
    tone?: string;
  };

  const confirmedAt = g.confirmed_at ?? 0;
  const completedAt = g.completed_at ?? 0;
  const placedAt = g.placed_at ?? 0;

  let terminal: Step;
  switch (g.status) {
    case "confirmed":
      // Landed-but-never-matched ends the pipeline in a neutral "In
      // library", not a green "Confirmed" — the file is filed, but no
      // StashDB scene was ever linked to it.
      terminal = isUnmatched(g)
        ? { label: "In library", at: confirmedAt, done: true, tone: "unmatched" }
        : { label: "Confirmed", at: confirmedAt, done: true, tone: "confirmed" };
      break;
    case "mismatched":
      terminal = { label: "Mismatched", at: confirmedAt, done: true, tone: "mismatched" };
      break;
    case "orphaned":
      terminal = { label: "Orphaned", at: 0, done: true, tone: "orphaned" };
      break;
    case "failed":
      terminal = { label: "Failed", at: 0, done: true, tone: "failed" };
      break;
    case "deferred":
      terminal = { label: "Retry scheduled", at: 0, done: false, active: true };
      break;
    case "scanned":
      terminal = { label: "Identifying", at: 0, done: false, active: true };
      break;
    case "tagging":
      terminal = { label: "Re-indexing", at: 0, done: false, active: true };
      break;
    case "distributing":
      terminal = { label: "Sorting", at: 0, done: false, active: true };
      break;
    default:
      terminal = { label: "Confirmed", at: 0, done: false, active: g.status === "placed" };
  }

  const steps: Step[] = [
    { label: "Grabbed", at: g.grabbed_at, done: g.grabbed_at > 0 },
    { label: "Downloaded", at: completedAt, done: completedAt > 0 },
    { label: "Placed", at: placedAt, done: placedAt > 0 },
    terminal,
  ];

  return (
    <ol className="grab-pipe">
      {steps.map((s, i) => {
        const linked = i > 0 && s.done && steps[i - 1].done;
        const cls =
          "grab-pipe-step" +
          (s.done ? " done" : "") +
          (s.active ? " active" : "") +
          (linked ? " linked" : "") +
          (s.tone ? " tone-" + s.tone : "");
        return (
          <li key={i} className={cls}>
            <span className="grab-pipe-node" />
            <span className="grab-pipe-label">{s.label}</span>
            <span className="grab-pipe-time">
              {s.at > 0 ? relativeTime(s.at) : " "}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

const stashdbScene = (id: string) => `https://stashdb.org/scenes/${id}`;

// grabMatchName mirrors the daemon's grabMatchName: the name the matcher
// works from — curated release title, else the placed file's basename,
// else the download client's name. Shown as the modal subtitle so it's
// clear what the candidates were ranked against.
function grabMatchName(g: Grab): string {
  if (g.release_title) return g.release_title;
  if (g.placed_path) {
    const parts = g.placed_path.split(/[/\\]/);
    return parts[parts.length - 1] || g.placed_path;
  }
  return g.client_name || "";
}

// confStrength buckets a 0..1 match confidence for color coding. The
// matcher's scores run low and flat for bare studio-code filenames (only
// the studio token matches), so these thresholds stay deliberately gentle
// — "mid" is still worth a look, the badge just signals relative footing.
function confStrength(c: number): "high" | "mid" | "low" {
  if (c >= 0.6) return "high";
  if (c >= 0.3) return "mid";
  return "low";
}

// MatchCandidateCard renders one ranked StashDB scene: cover, title,
// studio/date, performers, and the matching tokens that earned its score.
// Built to be scanned by eye — for cryptic filenames the right scene is
// in the list but not always #1, so title + performers + cover carry the
// decision, not the confidence number alone.
function MatchCandidateCard({
  c,
  rank,
  busy,
  onApply,
}: {
  c: MatchCandidate;
  rank: number;
  busy: boolean;
  onApply: () => void;
}) {
  const [imgFailed, setImgFailed] = useState(false);
  const pct = Math.round((c.confidence || 0) * 100);
  return (
    <div
      className="match-cand"
      style={{ animationDelay: `${Math.min(rank, 8) * 45}ms` }}
    >
      <div className="match-cand-poster">
        {c.image && !imgFailed ? (
          <img
            src={c.image}
            alt=""
            loading="lazy"
            onError={() => setImgFailed(true)}
          />
        ) : (
          <span className="match-cand-noposter">no cover</span>
        )}
      </div>

      <div className="match-cand-body">
        <a
          className="match-cand-title"
          href={stashdbScene(c.scene_id)}
          target="_blank"
          rel="noopener noreferrer"
          title="Open on StashDB"
        >
          {c.title || c.scene_id}
        </a>
        <div className="match-cand-meta">
          {c.studio && <span className="match-cand-studio">{c.studio}</span>}
          {c.date && <span className="match-cand-date">{c.date}</span>}
        </div>
        {c.performers && c.performers.length > 0 && (
          <div className="match-cand-perfs">
            {c.performers.map((p) => (
              <span className="match-cand-perf" key={p}>
                {p}
              </span>
            ))}
          </div>
        )}
        {c.reasons && c.reasons.length > 0 && (
          <div className="match-cand-tokens">
            {c.reasons.map((r, i) => (
              <span className="match-cand-token" key={i}>
                {r}
              </span>
            ))}
          </div>
        )}
      </div>

      <div className="match-cand-side">
        <div
          className="match-cand-conf"
          data-strength={confStrength(c.confidence || 0)}
          title="Match confidence"
        >
          <span className="match-cand-conf-num">{pct}</span>
          <span className="match-cand-conf-pct">%</span>
        </div>
        <button
          className="grab-action match"
          disabled={busy}
          onClick={onApply}
        >
          {busy ? "Applying…" : "Use this"}
        </button>
      </div>
    </div>
  );
}

// MatchCandidatesModal is the picker overlay: a ranked list of StashDB
// scenes for a grab phash couldn't link. Read-only suggestions — picking
// one calls the existing match endpoint via onApply. Portal + backdrop +
// Escape, matching the Settings modal's interaction model.
function MatchCandidatesModal({
  grabName,
  candidates,
  loading,
  error,
  applyBusy,
  applyErr,
  onApply,
  onClose,
}: {
  grabName: string;
  candidates: MatchCandidate[] | null;
  loading: boolean;
  error: string | null;
  applyBusy: boolean;
  applyErr: string | null;
  onApply: (sceneId: string) => void;
  onClose: () => void;
}) {
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const empty =
    !loading && !error && candidates !== null && candidates.length === 0;

  return createPortal(
    <div className="match-modal" onClick={onClose}>
      <div
        className="match-panel"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label="StashDB match candidates"
      >
        <div className="match-head">
          <div className="match-head-text">
            <h2>Match to StashDB</h2>
            <p className="match-sub" title={grabName}>
              {grabName || "this grab"}
            </p>
          </div>
          <button
            className="settings-close"
            onClick={onClose}
            aria-label="Close"
          >
            ×
          </button>
        </div>

        <div className="match-body">
          {loading && (
            <div className="match-state">
              <span className="match-spinner" aria-hidden="true" />
              Searching StashDB…
            </div>
          )}
          {error && <div className="match-state err">{error}</div>}
          {empty && (
            <div className="match-state">
              No candidates from this name. The filename has nothing the
              matcher can lock onto — close this and paste a StashDB scene
              URL instead.
            </div>
          )}
          {!loading &&
            !error &&
            candidates &&
            candidates.map((c, i) => (
              <MatchCandidateCard
                key={c.scene_id}
                c={c}
                rank={i}
                busy={applyBusy}
                onApply={() => onApply(c.scene_id)}
              />
            ))}
        </div>

        {applyErr && <div className="match-foot-err">{applyErr}</div>}
      </div>
    </div>,
    document.body,
  );
}

// Two scenes, side by side, because that is the whole question.
//
// This used to print two bare UUIDs with an arrow between them. Nobody can
// look at 019faec1… → 019baa2e… and see what went wrong: the ids are the same
// length, the same alphabet, and neither says what film it is. Worse, the big
// image at the top of the card is the scene that LANDED, with nothing marking
// it as such, so the one picture on screen showed the wrong answer and looked
// like the right one.
//
// So both sides get a picture and a title, labelled by what they mean rather
// than by which database field they came from: what forage went out for, and
// what actually arrived.
function MismatchPanel({
  g,
  predicted,
  actual,
  conf,
}: {
  g: Grab;
  predicted: string;
  actual: string;
  conf: string | null;
}) {
  const [chosen, setChosen] = useState<string | null>(
    g.status === "confirmed" ? actual : null,
  );
  const [busy, setBusy] = useState("");
  const [err, setErr] = useState("");
  const [cast, setCast] = useState<string[]>([]);

  const choose = async (id: string) => {
    setBusy(id);
    setErr("");
    try {
      await matchGrab(g.id, id);
      setChosen(id);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="grab-mismatch">
      <div className="grab-mismatch-head">
        <svg className="grab-match-glyph" viewBox="0 0 40 40" aria-hidden="true">
          <circle className="ring" cx="20" cy="20" r="17" />
          <path className="x1" d="M14 14 L26 26" />
          <path className="x2" d="M26 14 L14 26" />
        </svg>
        <div>
          <div className="grab-match-hero-title">Different scene</div>
          <div className="grab-match-hero-sub">
            Stash identified the file as a different scene than this grab went
            out for. One of them is wrong. Pick the one the file really is.
          </div>
        </div>
      </div>
      {/* Both sides carry the same kind of button, because either can be the
          wrong one: forage can pick the wrong release, and Stash's phash can
          pick the wrong scene. Presenting only "keep what arrived" quietly
          asserts that Stash is never mistaken.

          And the choice stays reversible. It used to vanish the moment it was
          made (the tool row below is gated on the grab still being unresolved),
          so a mis-tap was final and the panel that showed you the mistake
          offered no way to correct it. */}
      <div className="grab-mismatch-pair">
        <SceneFace
          id={predicted}
          label="Went out for"
          badge={conf ?? undefined}
          tone="wanted"
          chosen={chosen === predicted}
          busy={busy === predicted}
          action="Stash got it wrong, it's this one"
          onChoose={() => choose(predicted)}
        />
        <SceneFace
          id={actual}
          label="What arrived"
          tone="got"
          onDisk
          chosen={chosen === actual}
          busy={busy === actual}
          action="Keep this one"
          onChoose={() => choose(actual)}
          onCast={setCast}
        />
      </div>
      {err && <div className="grab-delete-err">{err}</div>}
      {chosen === actual && (
        <div className="grab-mismatch-note">
          Tagged as the scene that arrived. The one you wanted goes back on the
          watchlist.
          <ReFileHint g={g} cast={cast} />
        </div>
      )}
      {chosen === predicted && (
        <div className="grab-mismatch-note">
          Tagged as the scene forage went out for. The file stays where it is,
          which is the folder it was placed in for that scene.
        </div>
      )}
    </div>
  );
}

// Where a kept scene actually belongs.
//
// Matching does not move anything: a grab is placed into a performer folder
// when it lands, chosen from the scene forage PREDICTED. So accepting a
// different scene leaves the file filed under someone who is not in it, which
// is how a Harley Love scene ends up in Scarlett Rosewood's folder. Nothing
// said so; this does, and offers the move.
function ReFileHint({ g, cast }: { g: Grab; cast: string[] }) {
  const [busy, setBusy] = useState("");
  const [done, setDone] = useState("");
  const [err, setErr] = useState("");
  const folder = g.performer_name || "";
  const wrongFolder =
    folder && cast.length > 0 && !cast.some((n) => sameName(n, folder));
  if (done) {
    return <div className="grab-mismatch-refile">Re-filed under {done}.</div>;
  }
  if (!wrongFolder) return null;

  const move = async (name: string) => {
    setBusy(name);
    setErr("");
    try {
      await setGrabPerformer(g.id, name);
      setDone(name);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="grab-mismatch-refile">
      <span>
        It is filed under <strong>{folder}</strong>, who is not in this scene.
      </span>
      {cast.map((n) => (
        <button
          key={n}
          type="button"
          className="grab-action match"
          disabled={!!busy}
          onClick={() => move(n)}
        >
          {busy === n ? "Moving…" : `File under ${n}`}
        </button>
      ))}
      {err && <span className="grab-delete-err">{err}</span>}
    </div>
  );
}

// Loose name comparison: the folder name came from forage's own performer
// record and the cast list from StashDB, so they agree on the person without
// always agreeing on punctuation or case.
function sameName(a: string, b: string) {
  const norm = (x: string) => x.toLowerCase().replace(/[^a-z0-9]/g, "");
  return norm(a) === norm(b);
}

// SceneFace resolves an id to a picture and a title.
function SceneFace({
  id,
  label,
  badge,
  tone,
  onDisk,
  chosen,
  busy,
  action,
  onChoose,
  onCast,
}: {
  id: string;
  label: string;
  badge?: string;
  tone: "wanted" | "got";
  onDisk?: boolean;
  chosen?: boolean;
  busy?: boolean;
  action?: string;
  onChoose?: () => void;
  onCast?: (names: string[]) => void;
}) {
  const [card, setCard] = useState<SceneCard | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    fetchSceneCard(id)
      .then((c) => {
        if (cancelled) return;
        setCard(c);
        // Handed up so the panel can tell whether the file is filed under
        // someone who is actually in the scene it kept.
        onCast?.(c.performers ?? []);
      })
      .catch(() => !cancelled && setFailed(true));
    return () => {
      cancelled = true;
    };
  }, [id]);

  const img = proxiedImageURL(card?.image_url) || "";
  return (
    <div className={"scene-face " + tone + (chosen ? " is-chosen" : "")}>
      <div className="scene-face-label">
        {label}
        {onDisk && <span className="scene-face-tag">on disk</span>}
        {chosen && <span className="scene-face-tag chosen">tagged as this</span>}
      </div>
      <div className="scene-face-body">
        {img ? (
          <img className="scene-face-img" src={img} alt="" loading="lazy" />
        ) : (
          <div className="scene-face-img scene-face-img-empty" aria-hidden="true" />
        )}
        <div className="scene-face-text">
          <div className="scene-face-title">
            {card?.title || (failed ? "Could not look this scene up" : "…")}
          </div>
          <div className="scene-face-meta">
            {[card?.date, card?.studio_name].filter(Boolean).join(" · ")}
          </div>
          {card?.performers && card.performers.length > 0 && (
            <div className="scene-face-perf">{card.performers.join(", ")}</div>
          )}
          <a
            className="scene-face-id"
            href={stashdbScene(id)}
            target="_blank"
            rel="noopener noreferrer"
          >
            {id}
          </a>
          {badge && <span className="grab-match-badge dim">{badge}</span>}
        </div>
      </div>
      {action && onChoose && !chosen && (
        <button
          type="button"
          className={"grab-action match scene-face-pick " + tone}
          onClick={onChoose}
          disabled={busy}
        >
          {busy ? "Applying…" : action}
        </button>
      )}
    </div>
  );
}

// Accepting the file for what it is.
//
// Matching the grab to the scene that actually arrived is not just a tidier
// label: it is what tells forage it never got the scene it wanted. While the
// grab sits at "mismatched" the daemon counts the PREDICTED scene as covered,
// which holds that watch closed. Resolving to the arrived scene drops that
// cover, and the watch for the scene you were after reopens on the next pass.
function KeepScene({ g, actual }: { g: Grab; actual: string }) {
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [err, setErr] = useState("");

  const keep = async () => {
    setBusy(true);
    setErr("");
    try {
      await matchGrab(g.id, actual);
      setDone(true);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  if (done) {
    return (
      <div className="grab-mismatch-keep">
        <span className="grab-mismatch-kept">
          Kept. Tagged as the scene that arrived, and the one you wanted goes
          back on the watchlist.
        </span>
      </div>
    );
  }
  return (
    <div className="grab-mismatch-keep">
      <button
        type="button"
        className="grab-action match keep"
        onClick={keep}
        disabled={busy}
      >
        {busy ? "Keeping…" : "Keep this one"}
      </button>
      <span className="grab-mismatch-keep-note">
        Tag the file as the scene that arrived and keep hunting for the one you
        wanted.
      </span>
      {err && <span className="grab-delete-err">{err}</span>}
    </div>
  );
}

// MatchBlock unifies predicted-vs-actual by outcome:
//   • match    → ONE green "match confirmed" hero (the prediction and
//                Stash's identification are the same scene, so there's
//                nothing to compare — collapse them into a single
//                emphatic positive indicator).
//   • mismatch → TWO cards: Predicted (neutral) + Actual (amber), so
//                the divergence is obvious and both ids are visible.
//   • pending  → one Predicted card with an awaiting/not-in-Stash tag.
function MatchBlock({ g }: { g: Grab }) {
  const predicted = g.predicted_stashdb_id;
  if (!predicted) return null;
  const actual = g.actual_stashdb_id;
  const conf =
    g.predicted_confidence != null && g.predicted_confidence > 0
      ? g.predicted_confidence.toFixed(2)
      : null;

  if (actual && actual === predicted) {
    return (
      <div className="grab-match-hero confirmed">
        <svg
          className="grab-match-glyph"
          viewBox="0 0 40 40"
          aria-hidden="true"
        >
          <circle className="ring" cx="20" cy="20" r="17" />
          <path className="tick" d="M12 20.5 L18 26 L28 14" />
        </svg>
        <div className="grab-match-hero-body">
          <div className="grab-match-hero-title">Match confirmed</div>
          <div className="grab-match-hero-sub">
            Stash identified the scene forage predicted ·{" "}
            <a href={stashdbScene(predicted)} target="_blank" rel="noopener noreferrer">
              {predicted}
            </a>
            {conf && <span className="grab-match-badge">{conf}</span>}
          </div>
        </div>
      </div>
    );
  }

  if (actual) {
    return <MismatchPanel g={g} predicted={predicted} actual={actual} conf={conf} />;
  }

  // Predicted, but Stash never linked the file to any StashDB scene. When
  // the grab has already settled as confirmed that's the FINAL answer, not
  // a wait — say so rather than promising a confirmation that will never
  // come. Give it the same "not identified" hero the adopted path uses so
  // the outcome reads at a glance, with the prediction kept as the lead
  // the Find-matches tool below can act on.
  if (g.status === "confirmed") {
    return (
      <div className="grab-match-hero unidentified">
        <svg className="grab-match-glyph" viewBox="0 0 40 40" aria-hidden="true">
          <circle className="ring" cx="20" cy="20" r="17" />
          <text className="qmark" x="20" y="28" textAnchor="middle">
            ?
          </text>
        </svg>
        <div className="grab-match-hero-body">
          <div className="grab-match-hero-title">No StashDB match</div>
          <div className="grab-match-hero-sub">
            In your library, but Stash never linked it to a scene. forage
            predicted{" "}
            <a
              href={stashdbScene(predicted)}
              target="_blank"
              rel="noopener noreferrer"
            >
              {predicted}
            </a>
            {conf && <span className="grab-match-badge dim">{conf}</span>} — use
            Find matches below to apply it or pick another.
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="grab-fact pending">
      <span className="grab-fact-k">Predicted</span>
      <span className="grab-fact-v grab-match-line">
        <a href={stashdbScene(predicted)} target="_blank" rel="noopener noreferrer">
          {predicted}
        </a>
        {conf && <span className="grab-match-badge">match {conf}</span>}
        <span className="grab-match-pending">
          ·{" "}
          {g.status === "orphaned" || g.status === "failed"
            ? "not in Stash"
            : "awaiting confirmation"}
        </span>
      </span>
    </div>
  );
}

// IdentifyBlock is the adopted-scene analogue of MatchBlock's hero.
// Adopted grabs carry no prediction (you added the torrent to qBit
// yourself), so there's nothing to "match" against — only whether Stash
// linked the file to a StashDB scene.
//
// The truth is the cross-id on the placed scene in Stash (detail's
// local_stashdb_id), NOT the grab's own actual_stashdb_id: an adopted
// single often settles "in library (scanned)" before Stash's identify
// lands, or you identify it by hand later, so the grab field stays blank
// while Stash already holds the link. We trust detail and only fall back
// to the grab field (which gives an instant green for forage-identified
// adoptions, no wait for detail). While detail is still loading and the
// grab field is blank we show a neutral "checking" state rather than
// flashing "not identified" at a scene that turns out to be linked.
function IdentifyBlock({
  g,
  detail,
  detailLoading,
}: {
  g: Grab;
  detail: GrabDetail | null;
  detailLoading: boolean;
}) {
  const linked = g.actual_stashdb_id || detail?.local_stashdb_id || "";

  if (linked) {
    return (
      <div className="grab-match-hero confirmed">
        <svg
          className="grab-match-glyph"
          viewBox="0 0 40 40"
          aria-hidden="true"
        >
          <circle className="ring" cx="20" cy="20" r="17" />
          <path className="tick" d="M12 20.5 L18 26 L28 14" />
        </svg>
        <div className="grab-match-hero-body">
          <div className="grab-match-hero-title">Identified in Stash</div>
          <div className="grab-match-hero-sub">
            Linked to a StashDB scene ·{" "}
            <a
              href={stashdbScene(linked)}
              target="_blank"
              rel="noopener noreferrer"
            >
              {linked}
            </a>
          </div>
        </div>
      </div>
    );
  }

  if (detailLoading && !detail) {
    return (
      <div className="grab-match-hero checking">
        <svg
          className="grab-match-glyph"
          viewBox="0 0 40 40"
          aria-hidden="true"
        >
          <circle className="ring" cx="20" cy="20" r="17" />
        </svg>
        <div className="grab-match-hero-body">
          <div className="grab-match-hero-title">Checking Stash…</div>
          <div className="grab-match-hero-sub">
            Looking up whether this scene is linked to StashDB.
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="grab-match-hero unidentified">
      <svg
        className="grab-match-glyph"
        viewBox="0 0 40 40"
        aria-hidden="true"
      >
        <circle className="ring" cx="20" cy="20" r="17" />
        <text className="qmark" x="20" y="28" textAnchor="middle">
          ?
        </text>
      </svg>
      <div className="grab-match-hero-body">
        <div className="grab-match-hero-title">Not identified</div>
        <div className="grab-match-hero-sub">
          In your library, but not linked to a StashDB scene yet. Use Find
          matches below to link it.
        </div>
      </div>
    </div>
  );
}

// initials reduces a performer name to a 1–2 letter monogram for the
// poster fallback when no portrait is available ("Brie Belle" → "BB").
function initials(name?: string): string {
  const parts = (name || "").trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

// Stacked-layers glyph marking a multi-scene pack grab.
function PackGlyph() {
  return (
    <svg
      className="grab-pack-glyph"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M12 2 2 7l10 5 10-5-10-5Z" />
      <path d="m2 17 10 5 10-5" />
      <path d="m2 12 10 5 10-5" />
    </svg>
  );
}

// ── Scene grouping ──────────────────────────────────────────────────
//
// A render item is either one standalone grab or a group of attempts at
// the same scene. We group by the grab's scene id (actual, once phash
// confirms, else predicted) so every retry / pick-another-release for one
// scene collapses into a single card — the answer to "how many torrents
// have I tried for this scene?". Packs and grabs with no scene id are never
// grouped (a pack is many scenes; a no-id grab can't be tied to one).

type SceneGroupItem = {
  kind: "group";
  key: string; // the shared scene id
  sceneId: string;
  sceneTitle?: string; // StashDB title, when the daemon resolved it
  performerName?: string;
  grabs: Grab[];
};
type SingleItem = { kind: "single"; grab: Grab };
type GrabListItem = SceneGroupItem | SingleItem;

// sceneKey returns the id two attempts at the same scene share, or "" when
// the grab can't be grouped (pack, or no predicted/actual scene id).
function sceneKey(g: Grab): string {
  if (g.kind === "pack") return "";
  return g.actual_stashdb_id || g.predicted_stashdb_id || "";
}

function groupGrabsByScene(grabs: Grab[]): GrabListItem[] {
  // First pass: bucket by scene key, preserving first-seen order so the
  // overall list keeps its newest-first ordering (the group lands where its
  // newest attempt would have been).
  const order: string[] = [];
  const buckets = new Map<string, Grab[]>();
  const singles: GrabListItem[] = [];
  const slot = new Map<string, number>(); // key → index into `items`
  const items: GrabListItem[] = [];

  for (const g of grabs) {
    const key = sceneKey(g);
    if (!key) {
      items.push({ kind: "single", grab: g });
      continue;
    }
    if (!buckets.has(key)) {
      buckets.set(key, []);
      order.push(key);
      slot.set(key, items.length);
      // Placeholder; resolved to single-or-group in the second pass.
      items.push({ kind: "single", grab: g });
    }
    buckets.get(key)!.push(g);
  }

  // Second pass: a bucket with 2+ attempts becomes a group card; a lone
  // attempt stays a plain row.
  for (const key of order) {
    const bucket = buckets.get(key)!;
    const idx = slot.get(key)!;
    if (bucket.length >= 2) {
      items[idx] = {
        kind: "group",
        key,
        sceneId: key,
        sceneTitle: bucket.find((g) => g.scene_title)?.scene_title,
        performerName: bucket.find((g) => g.performer_name)?.performer_name,
        grabs: bucket,
      };
    } else {
      items[idx] = { kind: "single", grab: bucket[0] };
    }
  }
  void singles;
  return items;
}

// liveOutcome buckets a status into what the summary line counts: "live"
// (still in flight), "done" (landed AND matched to a StashDB scene),
// "unmatched" (landed but never linked — confirmed with no scene id), or
// "dead" (failed / abandoned / matched the wrong scene). Mirrors
// IN_FLIGHT / OUTCOME, with confirmed split by isUnmatched so a group
// header doesn't claim an unidentified file as a clean result.
function attemptTally(grabs: Grab[]): {
  live: number;
  done: number;
  unmatched: number;
  dead: number;
} {
  let live = 0;
  let done = 0;
  let unmatched = 0;
  let dead = 0;
  for (const g of grabs) {
    if (g.status === "confirmed") {
      if (isUnmatched(g)) unmatched++;
      else done++;
    } else if (isActiveStatus(g.status) || g.status === "deferred") live++;
    // deferred is live: parked for automatic retry, not an outcome.
    else dead++; // mismatched / orphaned / failed
  }
  return { live, done, unmatched, dead };
}

// SceneGroup is the attempt-stack card: one bordered block per scene with a
// header (performer · scene link · attempt tally) wrapping the per-attempt
// GrabRows passed as children. The header summarises at a glance; each
// attempt keeps its full row behaviour (expand / retry / delete / pick
// another) untouched.
function SceneGroup({
  group,
  children,
}: {
  group: SceneGroupItem;
  children: ReactNode;
}) {
  // Collapsed by default: the header line + tally summarises the scene's
  // whole story; the per-attempt rows are detail on demand. (Grabs pages
  // with many multi-attempt scenes were walls of rows otherwise.)
  const [open, setOpen] = useState(false);
  const tally = attemptTally(group.grabs);
  const n = group.grabs.length;
  const parts: string[] = [];
  if (tally.live) parts.push(`${tally.live} in flight`);
  if (tally.done) parts.push(`${tally.done} matched`);
  if (tally.unmatched) parts.push(`${tally.unmatched} unmatched`);
  if (tally.dead) parts.push(`${tally.dead} dead`);
  return (
    <li className={"grab-scene-group" + (open ? " open" : "")}>
      <div
        className="gsg-head gsg-clickable"
        onClick={(e) => {
          // Links inside the header (StashDB) keep their own behavior.
          if ((e.target as HTMLElement).closest("a")) return;
          setOpen((o) => !o);
        }}
        role="button"
        aria-expanded={open}
        title={open ? "Collapse attempts" : "Show attempts"}
      >
        <span className="gsg-chevron">{open ? "▾" : "▸"}</span>
        <div className="gsg-id">
          <span className="gsg-count">{n}</span>
          <span className="gsg-count-label">attempts</span>
        </div>
        <div className="gsg-title">
          {group.performerName && (
            <span className="gsg-performer">{group.performerName}</span>
          )}
          {group.sceneTitle ? (
            <>
              <a
                className="gsg-scene-title"
                href={stashdbScene(group.sceneId)}
                target="_blank"
                rel="noopener noreferrer"
                title="View scene on StashDB"
              >
                {group.sceneTitle}
              </a>
            </>
          ) : (
            <a
              className="gsg-scene"
              href={stashdbScene(group.sceneId)}
              target="_blank"
              rel="noopener noreferrer"
              title="View scene on StashDB"
            >
              scene {group.sceneId.slice(0, 8)}
            </a>
          )}
        </div>
        {parts.length > 0 && <div className="gsg-tally">{parts.join(" · ")}</div>}
      </div>
      {open && <ul className="gsg-attempts">{children}</ul>}
    </li>
  );
}


// RefreshIcon — circular-arrows glyph as an svg (flat, matches the app's
// icon language), used by the performer-search re-sync button.
function RefreshIcon() {
  return (
    <svg
      viewBox="0 0 24 24"
      width="1em"
      height="1em"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M21 12a9 9 0 1 1-2.64-6.36" />
      <path d="M21 3v5h-5" />
    </svg>
  );
}

// PerformerChip — a reassignment option. Hovering shows the performer's
// portrait (via the daemon's image proxy) so you can visually confirm it's
// the right person before re-filing. Portrait-only by design: "is this who
// I mean?" is the whole question when reassigning. Portalled + fixed so the
// card's overflow can't clip it.
const CHIP_HOVER_DELAY_MS = 200;
function PerformerChip({
  stashId,
  name,
  favorite,
  disabled,
  variant,
  onPick,
}: {
  stashId: string;
  name: string;
  favorite?: boolean;
  disabled?: boolean;
  variant: "chip" | "result";
  onPick: () => void;
}) {
  const [anchor, setAnchor] = useState<DOMRect | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const cancel = () => {
    if (timer.current) {
      window.clearTimeout(timer.current);
      timer.current = undefined;
    }
  };
  useEffect(() => () => cancel(), []);
  const img = performerImageURL(stashId);
  return (
    <>
      <button
        className={"grab-setperf-" + variant}
        disabled={disabled}
        onClick={onPick}
        onMouseEnter={(e) => {
          if (!img) return;
          const rect = e.currentTarget.getBoundingClientRect();
          cancel();
          timer.current = window.setTimeout(
            () => setAnchor(rect),
            CHIP_HOVER_DELAY_MS,
          );
        }}
        onMouseLeave={() => {
          cancel();
          setAnchor(null);
        }}
        title={`File under ${name}`}
      >
        {favorite && <span className="grab-setperf-fav">★</span>}
        {name}
      </button>
      {anchor &&
        img &&
        createPortal(
          <ChipPortrait anchor={anchor} img={img} name={name} />,
          document.body,
        )}
    </>
  );
}

function ChipPortrait({
  anchor,
  img,
  name,
}: {
  anchor: DOMRect;
  img: string;
  name: string;
}) {
  const CARD_W = 150;
  const GAP = 8;
  const vw = window.innerWidth;
  let left = anchor.left;
  if (left + CARD_W > vw - GAP) left = vw - CARD_W - GAP;
  if (left < GAP) left = GAP;
  const showBelow = anchor.bottom + 220 < window.innerHeight;
  const top = showBelow ? anchor.bottom + GAP : anchor.top - GAP;
  return (
    <div
      className="grab-chip-portrait"
      style={{
        left,
        top,
        width: CARD_W,
        transform: showBelow ? "" : "translateY(-100%)",
      }}
    >
      <div className="perf-hovercard-img">
        <img
          src={img}
          alt=""
          onLoad={(e) => {
            // Same contract as Discover: stop the placeholder once the image
            // is there. Without this every card on the page keeps an infinite
            // CSS animation repainting behind a picture that already arrived.
            (e.currentTarget as HTMLImageElement)
              .parentElement?.classList.add("thumb-loaded");
          }}
          onError={(e) => {
            const img = e.currentTarget as HTMLImageElement;
            img.style.display = "none";
            // A thumbnail that failed is not still loading.
            img.parentElement?.classList.add("thumb-failed");
          }}
        />
      </div>
      <div className="grab-chip-portrait-name">{name}</div>
    </div>
  );
}

// resLabel buckets a file's pixel height into the resolution label the user
// thinks in. "?" when Stash didn't report a height.
function resLabel(c?: SceneCopy): string {
  const h = c?.height ?? 0;
  if (h >= 2160) return "2160p";
  if (h >= 1440) return "1440p";
  if (h >= 1080) return "1080p";
  if (h >= 720) return "720p";
  if (h >= 480) return "480p";
  if (h > 0) return `${h}p`;
  return "?";
}

// DupCopyRow renders one copy line in a duplicate compare: tag (yours/pack),
// resolution, size, and the path (truncated, full path on hover).
// PackPerformerReviewer lets the user add the pack's performer to the scenes
// Stash COULDN'T identify (amateur content), shown on an expanded pack grab.
// Identified/mismatched scenes are never offered — the endpoint returns only
// unidentified ones. Covers are default-checked; deselect a stray, then apply
// (additive — existing performers preserved). Self-fetches; renders nothing when
// there's nothing unidentified or stash is unreachable.
// How many pack scenes the grid shows before you ask for the rest. Three
// rows at the widest layout: enough to recognise the pack, short enough that
// the button underneath it is on the same screen.
const PACK_PREVIEW = 9;

function PackPerformerReviewer({
  grabId,
  performerName,
}: {
  grabId: number;
  performerName: string;
}) {
  const [data, setData] = useState<PackScenes | null>(null);
  const [loading, setLoading] = useState(true);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [tagged, setTagged] = useState<Set<string>>(new Set());
  const [applying, setApplying] = useState(false);
  const [err, setErr] = useState("");
  const [showAll, setShowAll] = useState(false);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    fetchPackScenes(grabId)
      .then((d) => {
        if (!alive) return;
        setData(d);
        setSelected(new Set(d.scenes.map((s) => s.scene_id)));
      })
      .catch(() => {}) // silent — stash down / not critical to the card
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [grabId]);

  if (loading || !data || data.scenes.length === 0) return null;

  const untagged = data.scenes.filter((s) => !tagged.has(s.scene_id));
  const shown = showAll ? untagged : untagged.slice(0, PACK_PREVIEW);
  const hiddenCount = showAll ? 0 : untagged.length - shown.length;
  const allDone = untagged.length === 0;
  const selIds = untagged
    .filter((s) => selected.has(s.scene_id))
    .map((s) => s.scene_id);

  const toggle = (id: string) =>
    setSelected((prev) => {
      const n = new Set(prev);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

  const apply = async () => {
    if (selIds.length === 0 || applying) return;
    setApplying(true);
    setErr("");
    try {
      await applyPackPerformer(grabId, selIds);
      setTagged((prev) => new Set([...prev, ...selIds]));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setApplying(false);
    }
  };

  return (
    <div className="grab-pack-tag">
      <div className="grab-pack-tag-head">
        {allDone ? (
          <span className="grab-pack-tag-done">
            Tagged ✓ — {data.scenes.length} unidentified scene
            {data.scenes.length === 1 ? "" : "s"} credited to {performerName}
          </span>
        ) : (
          <>
            {untagged.length} scene{untagged.length === 1 ? "" : "s"} Stash
            couldn&rsquo;t identify
            <span className="grab-pack-tag-sub">
              {": tag with "}
              <strong>{performerName}</strong>
              {!data.performer_resolvable &&
                " (not in your Stash library — can't apply)"}
            </span>
          </>
        )}
      </div>
      {!allDone && (
        <>
          {/* A preview, not the whole pack.
              A pack of 608 unidentified scenes rendered 608 cards, which is
              several thousand pixels of page to scroll past to reach the one
              button at the bottom, and 608 thumbnails for the browser to
              fetch as you go. Nine is enough to see WHAT the pack is, which
              is the only question this grid answers before you press the
              button. Everything stays selected either way: hiding a card
              does not deselect it, so the count on the button is the truth
              whether or not the rest are on screen. */}
          <div className="scene-grid">
            {shown.map((s) => {
              const sel = selected.has(s.scene_id);
              const img = proxiedImageURL(s.image_url) || "";
              return (
                <div
                  key={s.scene_id}
                  className={"scene-card selectable" + (sel ? " selected" : "")}
                  role="button"
                  tabIndex={0}
                  aria-pressed={sel}
                  onClick={() => toggle(s.scene_id)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") toggle(s.scene_id);
                  }}
                >
                  <span className="scene-check" aria-hidden="true">
                    {sel ? "✓" : ""}
                  </span>
                  <div className="scene-thumb">
                    {img ? (
                      <img
                        src={img}
                        alt=""
                        loading="lazy"
                        onLoad={(e) => {
                          // Same contract as Discover: stop the placeholder once the image
                          // is there. Without this every card on the page keeps an infinite
                          // CSS animation repainting behind a picture that already arrived.
                          (e.currentTarget as HTMLImageElement)
                            .parentElement?.classList.add("thumb-loaded");
                        }}
                        onError={(e) => {
                          const img = e.currentTarget as HTMLImageElement;
                          img.style.display = "none";
                          // A thumbnail that failed is not still loading.
                          img.parentElement?.classList.add("thumb-failed");
                        }}
                      />
                    ) : null}
                  </div>
                  <div className="scene-info">
                    <div className="title">{s.title || "(untitled)"}</div>
                  </div>
                </div>
              );
            })}
          </div>
          {hiddenCount > 0 && (
            <button
              type="button"
              className="grab-pack-more"
              onClick={() => setShowAll(true)}
            >
              Show all {untagged.length} scenes
              <span className="grab-pack-more-note">
                {hiddenCount} more, all still selected
              </span>
            </button>
          )}
          {showAll && untagged.length > PACK_PREVIEW && (
            <button
              type="button"
              className="grab-pack-more"
              onClick={() => setShowAll(false)}
            >
              Show fewer
            </button>
          )}
          <div className="grab-pack-tag-foot">
            <button
              className="collection-cta"
              disabled={
                applying ||
                selIds.length === 0 ||
                !data.performer_resolvable
              }
              onClick={apply}
            >
              {applying
                ? "Tagging…"
                : `Add ${performerName} to ${selIds.length} scene${selIds.length === 1 ? "" : "s"}`}
            </button>
            {err && <span className="grab-delete-err">{err}</span>}
          </div>
        </>
      )}
    </div>
  );
}

function DupCopyRow({ tag, copy }: { tag: string; copy?: SceneCopy }) {
  return (
    <div className="grab-dup-copy">
      <span className="grab-dup-tag">{tag}</span>
      <span className="grab-dup-res">{resLabel(copy)}</span>
      {copy?.size ? (
        <span className="grab-dup-size">{humanSize(copy.size)}</span>
      ) : null}
      {copy?.path ? (
        <span className="grab-dup-path" title={copy.path}>
          {copy.path}
        </span>
      ) : null}
    </div>
  );
}

// dupWords supplies the duplicate-review copy for the grab's kind. The
// daemon runs BOTH cases through the pack machinery — a single grab that
// confirms into a scene you already have is recorded with the same schema
// and the same keep:"pack" wire value, where "the pack's copy" is just
// this download's file (see maybeRecordSingleDuplicate). The wire contract
// stays as-is; only what the user reads changes, so a lone scene isn't
// described as something a pack delivered.
function dupWords(isPack: boolean) {
  return isPack
    ? {
        // What this grab's copy is called, everywhere it's referred to.
        newTag: "pack",
        keepNew: "Keep pack",
        keepExisting: "Keep yours",
        existingTag: "yours",
        armNew: "Delete pack copy?",
        sub: "scenes this pack delivered that you already have",
        badgeHint:
          "This pack delivered scenes you already have — open to choose which copy to keep",
        keptNew: "Kept the pack copy, removed the original",
        keptExisting: "Kept your copy, removed the pack's",
      }
    : {
        newTag: "new",
        keepNew: "Keep new",
        keepExisting: "Keep existing",
        existingTag: "existing",
        armNew: "Delete the new file?",
        sub: "this download is a scene your library already had",
        badgeHint:
          "This download is a scene you already have — open to choose which copy to keep",
        keptNew: "Kept the new copy, removed the original",
        keptExisting: "Kept the existing copy, removed the new one",
      };
}

// DupCard is one scene this grab duplicated. The "existing" side is the
// pre-existing library copy (the highest-resolution one when several
// exist); the "new" side is what this download delivered — a pack's file
// or a single grab's, worded by dupWords. Keeping one destroys the other;
// Keep both leaves everything and dismisses the item.
function DupCard({
  dup,
  isPack,
  busy,
  disabled,
  onResolve,
}: {
  dup: DuplicateReview;
  isPack: boolean;
  busy: boolean;
  disabled: boolean;
  onResolve: (keep: "existing" | "pack" | "both") => void;
}) {
  const w = dupWords(isPack);
  // Representative existing copy = highest resolution, so "yours" reflects the
  // best file the user already holds.
  const best = [...dup.existing].sort(
    (a, b) => (b.height ?? 0) - (a.height ?? 0),
  )[0];
  const others = dup.existing.length - 1;
  // Keeping the existing copy destroys the new file, and keeping the new
  // file destroys the original(s) — both irreversible (file deleted
  // server-side), so they use
  // the same two-step arm as the grab delete button: first click flips the
  // label to spell out what gets deleted, second click within 4s commits.
  // "Keep both" destroys nothing and stays one click.
  const [armed, setArmed] = useState<"existing" | "pack" | null>(null);
  const armTimer = useRef<number | undefined>(undefined);
  useEffect(() => {
    return () => {
      if (armTimer.current) clearTimeout(armTimer.current);
    };
  }, []);
  function resolveArmed(keep: "existing" | "pack") {
    if (armed !== keep) {
      setArmed(keep);
      if (armTimer.current) clearTimeout(armTimer.current);
      armTimer.current = window.setTimeout(() => setArmed(null), 4000);
      return;
    }
    if (armTimer.current) clearTimeout(armTimer.current);
    setArmed(null);
    onResolve(keep);
  }
  return (
    <div className="grab-dup-card">
      <div className="grab-dup-title">
        {dup.scene_title || dup.stashdb_id}
      </div>
      <div className="grab-dup-compare">
        <DupCopyRow tag={w.existingTag} copy={best} />
        <DupCopyRow tag={w.newTag} copy={dup.pack} />
        {others > 0 && (
          <div className="grab-dup-more">
            + {others} other cop{others === 1 ? "y" : "ies"} already in your
            library
          </div>
        )}
      </div>
      <div className="grab-dup-actions">
        <button
          className={
            "grab-action" + (armed === "existing" ? " delete confirm" : "")
          }
          disabled={disabled}
          onClick={() => resolveArmed("existing")}
        >
          {busy ? "…" : armed === "existing" ? w.armNew : w.keepExisting}
        </button>
        <button
          className={
            "grab-action" + (armed === "pack" ? " delete confirm" : "")
          }
          disabled={disabled}
          onClick={() => resolveArmed("pack")}
        >
          {armed === "pack"
            ? `Delete your original${others > 0 ? "s" : ""}?`
            : w.keepNew}
        </button>
        <button
          className="grab-action ghost"
          disabled={disabled}
          onClick={() => onResolve("both")}
        >
          Keep both
        </button>
      </div>
    </div>
  );
}

function GrabRow({
  g,
  expanded,
  onToggle,
  onDeleted,
  onMatched,
  onRetried,
  onPerformerSet,
  onResolvedDuplicate,
  onPickScene,
}: {
  g: Grab;
  expanded: boolean;
  onToggle: () => void;
  onDeleted: (res: DeleteGrabResult) => void;
  onMatched: () => void;
  onRetried: () => void;
  onPerformerSet: (name: string) => void;
  onResolvedDuplicate: (msg: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [retrying, setRetrying] = useState(false);
  const [retryErr, setRetryErr] = useState<string | null>(null);
  async function handleRetry() {
    setRetrying(true);
    setRetryErr(null);
    try {
      await retryGrab(g.id);
      onRetried(); // parent refreshes; row re-renders as queued
    } catch (e) {
      setRetryErr((e as Error).message);
      setRetrying(false);
    }
  }
  const [detail, setDetail] = useState<GrabDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  // What deleting this grab will actually remove — the server-resolved plan,
  // fetched when the button arms so the confirm click is informed consent.
  const [delPreview, setDelPreview] = useState<GrabDeletePreview | null>(null);
  const [delPreviewErr, setDelPreviewErr] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const [matchVal, setMatchVal] = useState("");
  const [matchBusy, setMatchBusy] = useState(false);
  const [matchErr, setMatchErr] = useState<string | null>(null);
  // Candidate-picker modal: a ranked list of StashDB scenes the matcher
  // found from the grab's own name, for files phash couldn't link. Lazy —
  // fetched the first time the user opens it, then cached on the row.
  const [candOpen, setCandOpen] = useState(false);
  const [candidates, setCandidates] = useState<MatchCandidate[] | null>(null);
  const [candLoading, setCandLoading] = useState(false);
  const [candErr, setCandErr] = useState<string | null>(null);
  const [posterFailed, setPosterFailed] = useState(false);

  // Performer reassignment — re-files an Unsorted / mis-identified grab.
  const [perfBusy, setPerfBusy] = useState(false);
  const [perfErr, setPerfErr] = useState<string | null>(null);
  const [perfQuery, setPerfQuery] = useState("");
  const [perfResults, setPerfResults] = useState<
    { stash_id: string; name: string }[]
  >([]);
  async function applyPerformer(name: string) {
    const n = name.trim();
    if (!n || perfBusy) return;
    setPerfBusy(true);
    setPerfErr(null);
    try {
      await setGrabPerformer(g.id, n);
      onPerformerSet(n); // parent refreshes + shows "Filed under <name>"
    } catch (e) {
      setPerfErr((e as Error).message);
      setPerfBusy(false);
    }
  }
  // Re-runnable performer search (so the refresh button can re-query after
  // syncing the cache, not just on keystroke). Returns the matches.
  const [perfSearching, setPerfSearching] = useState(false);
  async function runPerfSearch(q: string) {
    if (q.trim().length < 2) {
      setPerfResults([]);
      return;
    }
    try {
      const r = await fetchPerformers({ q: q.trim() });
      setPerfResults(r.performers.slice(0, 6));
    } catch {
      setPerfResults([]);
    }
  }
  // Debounced performer search for the free-text box (anything the
  // suggestions didn't surface).
  useEffect(() => {
    const q = perfQuery.trim();
    if (q.length < 2) {
      setPerfResults([]);
      return;
    }
    let alive = true;
    const t = window.setTimeout(() => {
      if (alive) void runPerfSearch(q);
    }, 250);
    return () => {
      alive = false;
      clearTimeout(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [perfQuery]);
  // Force a Stash performer re-sync, then re-run the current search so a
  // just-created performer appears without waiting for the 6h cache tick.
  // perfSyncMsg gives explicit feedback (the icon spin alone was too subtle).
  const [perfSyncMsg, setPerfSyncMsg] = useState<string | null>(null);
  async function refreshPerfCache() {
    if (perfSearching) return;
    setPerfSearching(true);
    setPerfErr(null);
    setPerfSyncMsg("Syncing performers from Stash…");
    try {
      await refreshPerformers();
      await runPerfSearch(perfQuery);
      setPerfSyncMsg("Synced ✓ — performers up to date");
      window.setTimeout(() => setPerfSyncMsg(null), 4000);
    } catch (e) {
      setPerfSyncMsg(null);
      setPerfErr((e as Error).message);
    } finally {
      setPerfSearching(false);
    }
  }
  // Duplicate review (PackDedupKeep="review"): resolve a scene the pack
  // delivered that the library already had. dupBusy holds the in-flight dup
  // id; resolvedDups optimistically hides rows the user just decided so the
  // section collapses without waiting for the next list poll.
  const [dupBusy, setDupBusy] = useState<number | null>(null);
  const [dupErr, setDupErr] = useState<string | null>(null);
  const [resolvedDups, setResolvedDups] = useState<Set<number>>(new Set());
  async function resolveDup(
    dupId: number,
    keep: "existing" | "pack" | "both",
  ) {
    if (dupBusy != null) return;
    setDupBusy(dupId);
    setDupErr(null);
    try {
      const res = await resolveDuplicate(dupId, keep);
      if (!res.ok) {
        throw new Error((res.errors ?? []).join("; ") || "resolve failed");
      }
      setResolvedDups((s) => new Set(s).add(dupId));
      const w = dupWords(g.kind === "pack");
      onResolvedDuplicate(
        keep === "both"
          ? "Kept both copies"
          : keep === "pack"
            ? w.keptNew
            : w.keptExisting,
      );
    } catch (e) {
      setDupErr((e as Error).message);
    } finally {
      setDupBusy(null);
    }
  }

  const confirmTimer = useRef<number | undefined>(undefined);
  const fetchedDetail = useRef(false);

  async function handleMatch(useId?: string) {
    setMatchBusy(true);
    setMatchErr(null);
    try {
      await matchGrab(g.id, useId);
      setCandOpen(false); // close the picker if the pick came from there
      onMatched(); // parent refreshes; row re-renders as confirmed
    } catch (e) {
      setMatchErr((e as Error).message);
      setMatchBusy(false);
    }
  }

  // openCandidates lazily loads the matcher's ranked guesses the first
  // time, then just reopens the cached list. Errors surface inside the
  // modal so the picker still opens (with a retry message) on failure.
  async function openCandidates() {
    setCandOpen(true);
    if (candidates || candLoading) return;
    setCandLoading(true);
    setCandErr(null);
    try {
      const r = await fetchGrabMatchCandidates(g.id);
      setCandidates(r.candidates);
    } catch (e) {
      setCandErr((e as Error).message);
    } finally {
      setCandLoading(false);
    }
  }

  // Offer manual StashDB matching when phash didn't cleanly link the
  // file: a wrong-scene mismatch, or anything placed that never got a
  // StashDB cross-id (e.g. "in library (scanned)" — no fingerprint on
  // StashDB). Needs the file on disk to apply onto.
  // A divergence that has been resolved can be resolved differently: the two
  // ids still disagree, so the question is still open, whatever the status
  // says. Gating this on "mismatched" alone meant the first answer was the
  // last one, and the panel above showed the mistake with no way to fix it.
  const canMatch =
    !!g.placed_path &&
    (g.status === "mismatched" ||
      !g.actual_stashdb_id ||
      (!!g.predicted_stashdb_id && g.actual_stashdb_id !== g.predicted_stashdb_id));

  // Confirmed, but nothing on StashDB was ever linked to it — toned apart
  // from a real match everywhere the row shows its status.
  const unmatched = isUnmatched(g);

  // Fetch the rich detail (scene thumbnail/title/performers + local
  // Stash link) the first time the row opens, exactly once. A ref
  // guards the one-shot — deliberately NOT detailLoading state in the
  // dep array, which would make the effect re-run the instant it set
  // loading, fire its own cleanup, and cancel the in-flight fetch
  // (leaving "Loading scene…" stuck forever).
  useEffect(() => {
    if (!expanded || fetchedDetail.current) return;
    fetchedDetail.current = true;
    setDetailLoading(true);
    fetchGrabDetail(g.id)
      .then(setDetail)
      .catch(() => {
        /* card just renders without the hero */
      })
      .finally(() => setDetailLoading(false));
  }, [expanded, g.id]);

  useEffect(() => {
    return () => {
      if (confirmTimer.current) clearTimeout(confirmTimer.current);
    };
  }, []);

  async function handleDelete() {
    // Two-step: first click arms and loads the deletion preview; second
    // click commits. The window is longer than the plain arm used to be
    // because there is now a file list to read.
    if (!confirmDelete) {
      setConfirmDelete(true);
      setDelPreview(null);
      setDelPreviewErr(null);
      fetchGrabDeletePreview(g.id)
        .then(setDelPreview)
        .catch((e) => setDelPreviewErr((e as Error).message));
      confirmTimer.current = window.setTimeout(() => {
        setConfirmDelete(false);
        setDelPreview(null);
        setDelPreviewErr(null);
      }, 12000);
      return;
    }
    if (confirmTimer.current) clearTimeout(confirmTimer.current);
    setConfirmDelete(false);
    setDelPreview(null);
    setDelPreviewErr(null);
    setDeleting(true);
    setDeleteErr(null);
    try {
      const res = await deleteGrab(g.id);
      onDeleted(res); // parent refreshes the list; row unmounts
    } catch (e) {
      setDeleteErr((e as Error).message);
      setDeleting(false);
    }
  }

  return (
    <li
      className={
        "grab-row status-" +
        g.status +
        (unmatched ? " unmatched" : "") +
        (expanded ? " open" : "")
      }
    >
      <button className="grab-row-head" onClick={onToggle}>
        <div className="grab-row-badges">
          <span
            className={
              "grab-status-badge chip-" + g.status + (unmatched ? " unmatched" : "")
            }
          >
            {g.status}
          </span>
          {unmatched && (
            <span
              className="grab-nomatch-badge"
              title="In your library, but no StashDB scene was ever linked to it — open the row and use Find matches to link it"
            >
              NO MATCH
            </span>
          )}
          {g.stalled && (
            <span
              className="grab-stalled-badge"
              title="No download progress for a while — try abandoning it and picking another release"
            >
              STALLED
            </span>
          )}
          {g.place_failing && (
            <span
              className="grab-placefail-badge"
              title="Downloaded, but can't place it into the library (check the place error below — permission / mount / path). Still retrying."
            >
              PLACE FAILING
            </span>
          )}
          {g.kind === "pack" && (
            <span className="grab-pack-badge" title="Multi-scene pack">
              <PackGlyph />
              PACK
            </span>
          )}
          {(g.pending_duplicates ?? 0) > 0 && (
            <span
              className="grab-dups-badge"
              title={dupWords(g.kind === "pack").badgeHint}
            >
              {g.pending_duplicates} TO REVIEW
            </span>
          )}
          {g.adopted && (
            <span
              className="grab-adopt-badge"
              title="Adopted — you added this to qBit directly (forager category); forage picked it up"
            >
              ADOPTED
            </span>
          )}
        </div>
        <div className="grab-row-body">
          <div className="grab-title">{g.release_title}</div>
          <div className="grab-meta">
            {g.performer_name && <span>{g.performer_name}</span>}
            {g.performer_name && <span className="sep">·</span>}
            <span>{g.client || "?"}</span>
            {g.release_indexer && (
              <>
                <span className="sep">·</span>
                <span>{g.release_indexer}</span>
              </>
            )}
            {g.release_size != null && g.release_size > 0 && (
              <>
                <span className="sep">·</span>
                <span>{humanSize(g.release_size)}</span>
              </>
            )}
            {g.kind === "pack" && (
              <>
                <span className="sep">·</span>
                <span className="grab-pack-prog">
                  {g.pack_identified ?? 0}/{g.pack_files || "?"} identified
                  {(g.pack_deduped ?? 0) > 0 && ` · ${g.pack_deduped} removed`}
                </span>
              </>
            )}
            <span className="sep">·</span>
            <span>{relativeTime(g.updated_at)}</span>
          </div>
        </div>
        <span
          className={"grab-caret fchev" + (expanded ? " open" : "")}
          aria-hidden="true"
        />
      </button>

      {expanded && (
        <div className="grab-row-detail">
          {/* Dossier: left aside (poster · progress · actions) +
              identity/pipeline main column. */}
          <div className="grab-dossier">
            {(() => {
              // A pack is many of a performer's scenes — no single
              // thumbnail represents it, so lead with the performer
              // portrait. A single is one scene: prefer the StashDB
              // predicted thumbnail (clean promo image, available the
              // instant the scene is predicted at search time), then the
              // placed file's own Stash screenshot (covers grabs with no
              // StashDB match), and only then the performer portrait.
              const isPack = g.kind === "pack";
              // performer_image_url / local_scene_image_url are daemon-
              // relative (/img/...) — resolve them through the image proxy.
              // image_url is an absolute StashDB CDN URL (passes through).
              const perfImg = proxiedImageURL(detail?.performer_image_url) || "";
              const sceneShot =
                proxiedImageURL(detail?.local_scene_image_url) || "";
              const stashdbImg = proxiedImageURL(detail?.image_url) || "";
              const heroSrc = posterFailed
                ? ""
                : isPack
                  ? perfImg || stashdbImg || sceneShot || ""
                  : stashdbImg || sceneShot || perfImg || "";
              // A scene image is 16:9; a performer portrait is 3:4. Frame
              // the poster to match whichever the hero actually is so a
              // scene thumbnail isn't cropped into a portrait (and vice
              // versa). The performer portrait is the only non-scene
              // source, so anything else is a scene image.
              const isScene = !!heroSrc && heroSrc !== perfImg;
              const posterClass =
                "grab-poster" +
                (heroSrc ? (isScene ? " is-scene" : "") : " is-mono");
              return (
                <div className={posterClass}>
                  {heroSrc ? (
                    <img
                      src={heroSrc}
                      alt={g.performer_name || ""}
                      loading="lazy"
                      onError={() => setPosterFailed(true)}
                    />
                  ) : (
                    <span className="grab-poster-initials" aria-hidden="true">
                      {detailLoading ? "" : initials(g.performer_name)}
                    </span>
                  )}
                  <span
                    className={
                      "grab-poster-badge chip-" +
                      g.status +
                      (unmatched ? " unmatched" : "")
                    }
                  >
                    {unmatched ? "no match" : g.status}
                  </span>
                  {/* Name caption only on the performer-portrait (pack /
                      monogram) view — a scene thumbnail names itself via
                      the title beside it. */}
                  {g.performer_name && (isPack || !heroSrc) && (
                    <span className="grab-poster-name">{g.performer_name}</span>
                  )}
                </div>
              );
            })()}

            <div className="grab-dossier-main">
              <h3 className="grab-ident-title">
                {detail?.title || g.release_title}
              </h3>
              {(detail?.date || detail?.studio) && (
                <div className="grab-ident-sub">
                  {detail.date && <span>{detail.date}</span>}
                  {detail.date && detail.studio && (
                    <span className="grab-ident-dot" />
                  )}
                  {detail.studio && <span>{detail.studio}</span>}
                </div>
              )}
              {detail && detail.performers.length > 0 && (
                <div className="grab-perf-chips">
                  {detail.performers.map((p, i) => (
                    <span className="grab-perf-chip" key={i}>
                      {p.name}
                    </span>
                  ))}
                </div>
              )}
              {g.reason && <div className="grab-reason">{g.reason}</div>}
              {g.status === "deferred" && (
                <div className="grab-reason">
                  attempt {(g.attempts ?? 0) + 1}/{g.max_attempts ?? 5}
                  {g.fail_kind === "client"
                    ? " · download client offline"
                    : g.fail_kind === "indexer"
                      ? " · indexer trouble"
                      : ""}{" "}
                  {retryEta(g.next_retry_at)}
                </div>
              )}

              <Pipeline g={g} />
            </div>
          </div>

          {/* Duplicate review (PackDedupKeep="review") — scenes this pack
              delivered that the library already had. The user keeps the
              better copy per scene; the destroy runs server-side. */}
          {(() => {
            const dups = (detail?.duplicates ?? []).filter(
              (d) => !resolvedDups.has(d.id),
            );
            if (dups.length === 0) return null;
            return (
              <div className="grab-dups">
                <div className="grab-dups-head">
                  <span className="grab-dups-warn" aria-hidden="true">
                    !
                  </span>
                  {dups.length} duplicate{dups.length === 1 ? "" : "s"} to review
                  <span className="grab-dups-sub">
                    {dupWords(g.kind === "pack").sub}
                  </span>
                </div>
                {dups.map((d) => (
                  <DupCard
                    key={d.id}
                    dup={d}
                    isPack={g.kind === "pack"}
                    busy={dupBusy === d.id}
                    disabled={dupBusy != null}
                    onResolve={(keep) => resolveDup(d.id, keep)}
                  />
                ))}
                {dupErr && <div className="grab-delete-err">{dupErr}</div>}
              </div>
            );
          })()}

          {/* Pack: tag the scenes Stash couldn't identify (amateur content)
              with the pack's performer. Renders only for pack grabs that have
              unidentified scenes. */}
          {g.kind === "pack" && g.performer_name && (
            <PackPerformerReviewer
              grabId={g.id}
              performerName={g.performer_name}
            />
          )}

          {/* Live download progress — full-width band, only in flight. */}
          {g.progress && (
            <div className="grab-progress">
              <div className="grab-progress-head">
                <span className="grab-progress-pct">
                  {g.progress.percent.toFixed(0)}%
                </span>
                <span className="grab-progress-rate">
                  {g.progress.speed_bps
                    ? `${humanSize(g.progress.speed_bps)}/s`
                    : "downloading"}
                  {g.progress.eta_secs
                    ? ` · ${humanDuration(g.progress.eta_secs)} left`
                    : ""}
                </span>
              </div>
              <div className="grab-progress-track">
                <div
                  className="grab-progress-fill"
                  style={{
                    width: `${Math.min(100, Math.max(0, g.progress.percent)).toFixed(0)}%`,
                  }}
                />
              </div>
            </div>
          )}

          {/* The record — labelled cards, not a flat list. */}
          <div className="grab-facts">
            {g.predicted_stashdb_id && <MatchBlock g={g} />}
            {!g.predicted_stashdb_id && g.placed_path && (
              <IdentifyBlock
                g={g}
                detail={detail}
                detailLoading={detailLoading}
              />
            )}
            {g.placed_path && (
              <div className="grab-fact">
                <span className="grab-fact-k">Placed</span>
                <code className="grab-fact-v">{g.placed_path}</code>
              </div>
            )}
            {g.client_name && (
              <div className="grab-fact">
                <span className="grab-fact-k">Client file</span>
                <code className="grab-fact-v">{g.client_name}</code>
              </div>
            )}
            {g.place_error && (
              <div className="grab-fact err">
                <span className="grab-fact-k">Place error</span>
                <span className="grab-fact-v">{g.place_error}</span>
              </div>
            )}
          </div>

          {/* Manual StashDB match — for files phash couldn't link. */}
          {canMatch && (
            <div className="grab-match-tool">
              <span className="grab-match-tool-label">
                {g.status === "mismatched"
                  ? "Wrong scene? Force the right StashDB match:"
                  : "Not on StashDB by fingerprint. Find it by name, or paste a link:"}
              </span>
              <div className="grab-match-tool-row">
                <button
                  className="grab-action match find"
                  onClick={openCandidates}
                  disabled={matchBusy}
                >
                  <svg
                    className="grab-match-find-glyph"
                    viewBox="0 0 24 24"
                    aria-hidden="true"
                  >
                    <circle cx="10.5" cy="10.5" r="6.5" />
                    <line x1="15.5" y1="15.5" x2="21" y2="21" />
                  </svg>
                  Find matches
                </button>
                {g.predicted_stashdb_id && (
                  <button
                    className="grab-action match"
                    onClick={() => handleMatch()}
                    disabled={matchBusy}
                  >
                    {matchBusy ? "Applying…" : "Use predicted scene"}
                  </button>
                )}
                <input
                  type="text"
                  className="grab-match-input"
                  placeholder="StashDB scene URL or id"
                  value={matchVal}
                  onChange={(e) => setMatchVal(e.target.value)}
                />
                <button
                  className="grab-action match"
                  disabled={matchBusy || !matchVal.trim()}
                  onClick={() => handleMatch(matchVal.trim())}
                >
                  Apply
                </button>
                {matchErr && (
                  <span className="grab-delete-err">{matchErr}</span>
                )}
              </div>
            </div>
          )}

          {candOpen && (
            <MatchCandidatesModal
              grabName={grabMatchName(g)}
              candidates={candidates}
              loading={candLoading}
              error={candErr}
              applyBusy={matchBusy}
              applyErr={matchErr}
              onApply={(sceneId) => handleMatch(sceneId)}
              onClose={() => setCandOpen(false)}
            />
          )}

          {/* Action bar */}
          {/* Set / change the performer folder — re-files the (still-
              seeding) download into <library>/<performer>/. The fix for an
              Unsorted or mis-identified grab. Packs included: forage files a
              whole pack into one performer folder, so it's reassignable too. */}
          <div className="grab-setperf">
              <span className="grab-setperf-label">
                {g.performer_name && !isUnfiledFolder(g.performer_name)
                  ? "Performer"
                  : "Set performer"}
                {g.performer_name && (
                  <span className="grab-setperf-current">
                    {g.performer_name}
                  </span>
                )}
              </span>
              <div className="grab-setperf-chips">
                {(detail?.performer_suggestions ?? [])
                  .filter((p) => p.name !== g.performer_name)
                  .slice(0, 5)
                  .map((p) => (
                    <PerformerChip
                      key={p.stash_id}
                      stashId={p.stash_id}
                      name={p.name}
                      favorite={p.favorite}
                      disabled={perfBusy}
                      variant="chip"
                      onPick={() => applyPerformer(p.name)}
                    />
                  ))}
                <span className="grab-setperf-searchwrap">
                  <input
                    className="grab-setperf-search"
                    type="text"
                    value={perfQuery}
                    placeholder="search…"
                    spellCheck={false}
                    disabled={perfBusy}
                    onChange={(e) => setPerfQuery(e.target.value)}
                  />
                  <button
                    type="button"
                    className={
                      "grab-setperf-refresh" + (perfSearching ? " spinning" : "")
                    }
                    onClick={refreshPerfCache}
                    disabled={perfBusy || perfSearching}
                    title="Re-sync performers from Stash (for one you just created)"
                    aria-label="Refresh performers from Stash"
                  >
                    <RefreshIcon />
                  </button>
                </span>
              </div>
              {(() => {
                // Dedup search results against the suggestion chips above
                // (and the current performer), so the same name doesn't
                // appear in both rows.
                const suggestedNames = new Set(
                  (detail?.performer_suggestions ?? [])
                    .slice(0, 5)
                    .map((p) => p.name.toLowerCase()),
                );
                const results = perfResults.filter(
                  (p) =>
                    p.name !== g.performer_name &&
                    !suggestedNames.has(p.name.toLowerCase()),
                );
                if (results.length === 0) return null;
                return (
                  <div className="grab-setperf-results">
                    <span className="grab-setperf-results-label">
                      Search results
                    </span>
                    <div className="grab-setperf-results-chips">
                      {results.map((p) => (
                        <PerformerChip
                          key={p.stash_id}
                          stashId={p.stash_id}
                          name={p.name}
                          disabled={perfBusy}
                          variant="result"
                          onPick={() => applyPerformer(p.name)}
                        />
                      ))}
                    </div>
                  </div>
                );
              })()}
              {perfSyncMsg && (
                <span className="grab-setperf-sync">{perfSyncMsg}</span>
              )}
              {perfBusy && <span className="grab-setperf-busy">Re-filing…</span>}
              {perfErr && <span className="grab-delete-err">{perfErr}</span>}
            </div>

          <div className="grab-actions">
            {/* Retry a failed grab from its stored download URL — for
                transient failures (a tracker download cap will just fail
                again; Pick another release is the fix there). A deferred
                grab retries itself on its backoff schedule; the button
                skips the wait and also resets the attempt budget. */}
            {(g.status === "failed" || g.status === "deferred") && (
              <button
                className="grab-action retry"
                onClick={handleRetry}
                disabled={retrying}
                title={
                  g.status === "deferred"
                    ? "Retry immediately instead of waiting for the scheduled attempt"
                    : "Re-attempt this release"
                }
              >
                {retrying
                  ? "Retrying…"
                  : g.status === "deferred"
                    ? "Retry now ↻"
                    : "Retry ↻"}
              </button>
            )}
            {retryErr && <span className="grab-delete-err">{retryErr}</span>}
            {/* The budget ran out on its own: tell the user what the
                buttons DO about it instead of leaving a dead end. */}
            {g.status === "failed" && (g.reason ?? "").includes("gave up after") && (
              <span className="grab-retry-hint">
                auto-retry exhausted · Retry restarts the budget; if the
                source is dead, pick another release
              </span>
            )}
            {/* When a grab stalled or didn't land cleanly, jump back to
                the scene's releases to pick a different one. (Deferred
                grabs auto-retry the SAME release on a backoff schedule;
                this is the escape hatch to a different release.) */}
            {g.predicted_stashdb_id &&
              (g.stalled ||
                g.status === "failed" ||
                g.status === "deferred" ||
                g.status === "mismatched" ||
                g.status === "orphaned") && (
                <button
                  className="grab-action pick-another"
                  onClick={() =>
                    onPickScene(g.predicted_stashdb_id!, g.performer_name)
                  }
                >
                  Pick another release →
                </button>
              )}
            {detail?.stash_scene_url && (
              <a
                className="grab-action open-stash"
                href={detail.stash_scene_url}
                target="_blank"
                rel="noopener noreferrer"
              >
                Open in Stash ↗
              </a>
            )}
            <div className="grab-actions-right">
              {deleteErr && <span className="grab-delete-err">{deleteErr}</span>}
              <button
                className={
                  "grab-action delete" + (confirmDelete ? " confirm" : "")
                }
                onClick={handleDelete}
                disabled={deleting}
              >
                {deleting
                  ? "Deleting…"
                  : confirmDelete
                    ? "Confirm delete?"
                    : "Delete"}
              </button>
            </div>
          </div>
          {confirmDelete && (
            <DeletePreviewPanel preview={delPreview} err={delPreviewErr} />
          )}
        </div>
      )}
    </li>
  );
}

// DeletePreviewPanel renders, under the armed Delete button, exactly what
// confirming will remove — the server-resolved plan the purge itself
// executes, so this list cannot drift from the action. Refusals ("kept")
// are shown too: knowing what a delete will NOT touch is half the safety.
function DeletePreviewPanel({
  preview,
  err,
}: {
  preview: GrabDeletePreview | null;
  err: string | null;
}) {
  if (err) {
    return (
      <div className="grab-del-preview">
        <div className="gdp-err">
          Couldn&rsquo;t load the deletion preview ({err}). Confirming still
          deletes this grab&rsquo;s own file, download and record.
        </div>
      </div>
    );
  }
  if (!preview) {
    return (
      <div className="grab-del-preview">
        <div className="gdp-loading">Checking what this will delete…</div>
      </div>
    );
  }
  // Flatten to displayable rows; a pack can resolve to hundreds of files,
  // so cap the list and say how many more there are — a truncated preview
  // must never look complete.
  const rows: { key: string; path: string; size?: number; tag: string }[] = [];
  for (const sc of preview.scenes ?? []) {
    for (const f of sc.files) {
      rows.push({ key: "s:" + f.path, path: f.path, size: f.size, tag: "stash scene + file" });
    }
  }
  for (const p of preview.disk ?? []) {
    rows.push({ key: "d:" + p, path: p, tag: "disk" });
  }
  const MAX = 12;
  const shown = rows.slice(0, MAX);
  const more = rows.length - shown.length;
  const trashDays = preview.trash?.enabled ? preview.trash.kept_days : 0;
  return (
    <div className="grab-del-preview">
      <div className="gdp-title">
        {trashDays > 0
          ? `This will move to forage's trash (recoverable for ${trashDays} days):`
          : "This will permanently delete:"}
      </div>
      <ul className="gdp-list">
        {shown.map((r) => (
          <li key={r.key}>
            <code className="gdp-path" title={r.path}>
              {r.path}
            </code>
            {r.size ? <span className="gdp-size">{humanSize(r.size)}</span> : null}
            <span className="gdp-tag">{r.tag}</span>
          </li>
        ))}
        {more > 0 && (
          <li className="gdp-more">…and {more} more file{more === 1 ? "" : "s"}</li>
        )}
        {preview.client && (
          <li>
            <span className="gdp-clientline">{preview.client}</span>
          </li>
        )}
        <li>
          <span className="gdp-clientline">this grab&rsquo;s record in forage</span>
        </li>
      </ul>
      {(preview.kept ?? []).length > 0 && (
        <div className="gdp-kept">
          {(preview.kept ?? []).map((k) => (
            <div key={k.target.scene_id}>
              <strong>Not</strong> deleting scene {k.target.scene_id}:{" "}
              {k.reason}.
            </div>
          ))}
        </div>
      )}
      {(preview.notes ?? []).map((n, i) => (
        <div className="gdp-note" key={i}>
          {n}
        </div>
      ))}
      {trashDays > 0 && (
        <div className="gdp-note">
          Files the daemon can&rsquo;t reach are deleted permanently instead
          (recorded in the journal either way).
        </div>
      )}
    </div>
  );
}

function humanDuration(secs: number): string {
  if (secs <= 0) return "";
  if (secs < 60) return `${Math.round(secs)}s`;
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}


function relativeTime(unix: number): string {
  if (!unix) return "";
  const ageSec = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (ageSec < 60) return `${ageSec}s ago`;
  if (ageSec < 3600) return `${Math.floor(ageSec / 60)}m ago`;
  if (ageSec < 86_400) return `${Math.floor(ageSec / 3600)}h ago`;
  return `${Math.floor(ageSec / 86_400)}d ago`;
}
