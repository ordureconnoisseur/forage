import {
  Fragment,
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { humanSize } from "../format";
import {
  ACTIVE_STATUSES,
  adoptDownloads,
  retryFailedGrabs,
  deleteGrab,
  DeleteGrabResult,
  fetchGrabDetail,
  fetchGrabs,
  Grab,
  GrabDetail,
  grabTorrentFile,
  inspectTorrentFile,
  type TorrentInspect,
  matchGrab,
  retryGrab,
  setGrabPerformer,
  fetchPerformers,
  refreshPerformers,
  resolveDuplicate,
  type DuplicateReview,
  type SceneCopy,
  GrabsResponse,
  GrabStatus,
  isActiveStatus,
  proxiedImageURL,
  performerImageURL,
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
const OUTCOME: GrabStatus[] = [
  "confirmed",
  "mismatched",
  "orphaned",
  "failed",
];

export default function GrabsList({
  onPickScene,
}: {
  // Jump to a scene's releases view (to pick a different release when a
  // grab stalled or failed). Receives the scene's StashDB id + the
  // performer to place under.
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [data, setData] = useState<GrabsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<GrabStatus | "any">("any");
  const [q, setQ] = useState("");
  const [expanded, setExpanded] = useState<Set<number>>(new Set());
  const [notice, setNotice] = useState<string | null>(null);
  const [adopting, setAdopting] = useState(false);
  const [retryingAll, setRetryingAll] = useState(false);
  const [addOpen, setAddOpen] = useState(false);
  const [addFile, setAddFile] = useState<File | null>(null);
  const [addInspect, setAddInspect] = useState<TorrentInspect | null>(null);
  const [addInspecting, setAddInspecting] = useState(false);
  const [addName, setAddName] = useState("");
  const [addBusy, setAddBusy] = useState(false);
  const [addErr, setAddErr] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const lastFetch = useRef(0);

  // Immediate refetch, used after a delete so the row disappears
  // without waiting for the next poll tick.
  const refresh = useCallback(async () => {
    try {
      const r = await fetchGrabs({ limit: 200 });
      setData(r);
    } catch {
      /* next poll tick will recover */
    }
  }, []);

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
          ? `Adopted ${res.adopted} new download${res.adopted === 1 ? "" : "s"}`
          : "No new downloads to adopt",
      );
      await refresh();
    } catch (e) {
      setNotice("Scan failed: " + (e as Error).message);
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
    setAddFile(null);
    setAddInspect(null);
    setAddInspecting(false);
    setAddName("");
    setAddErr(null);
    if (fileRef.current) fileRef.current.value = "";
  }, []);

  // Picking a file inspects it (parse name/size/counts + suggest a
  // performer folder) before any download — confirm-first.
  const onPickTorrent = useCallback(async () => {
    const f = fileRef.current?.files?.[0] ?? null;
    setAddFile(f);
    setAddInspect(null);
    setAddName("");
    setAddErr(null);
    if (!f) return;
    setAddInspecting(true);
    try {
      const ins = await inspectTorrentFile(f);
      setAddInspect(ins);
      setAddName(ins.suggested_performers[0]?.name ?? "");
    } catch (e) {
      setAddErr((e as Error).message);
    } finally {
      setAddInspecting(false);
    }
  }, []);

  const submitTorrent = useCallback(async () => {
    if (!addFile) {
      setAddErr("choose a .torrent file first");
      return;
    }
    setAddBusy(true);
    setAddErr(null);
    try {
      const res = await grabTorrentFile(addFile, addName.trim());
      const label = addInspect?.name ? `"${addInspect.name}"` : "torrent";
      resetAddForm();
      setAddOpen(false);
      setNotice(`Added ${label} → grab #${res.grab_id}`);
      void refresh();
    } catch (e) {
      setAddErr((e as Error).message);
    } finally {
      setAddBusy(false);
    }
  }, [addFile, addName, addInspect, refresh, resetAddForm]);

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

  // Poll loop — uses setTimeout (not setInterval) so we can adapt the
  // delay between ticks based on whether there are active grabs.
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    let inFlight = false;

    async function tick() {
      if (cancelled || inFlight) return;
      if (document.hidden) {
        // Don't waste cycles when the tab isn't visible. Reschedule
        // and try again later — visibilitychange handler also kicks.
        timer = window.setTimeout(tick, SLOW_POLL_MS);
        return;
      }
      // The cadence decision reads the response we JUST fetched. This
      // effect mounts once with an empty dep list, so the `data` state
      // variable in this closure is the mount render's value (null,
      // forever) — deciding off it meant the fast cadence never engaged
      // and active downloads only refreshed every SLOW_POLL_MS.
      let hasActive = false;
      inFlight = true;
      try {
        const r = await fetchGrabs({ limit: 200 });
        if (cancelled) return;
        setData(r);
        setError(null);
        lastFetch.current = Date.now();
        hasActive = r.grabs.some((g) => isActiveStatus(g.status));
      } catch (e) {
        if (cancelled) return;
        setError((e as Error).message);
      } finally {
        inFlight = false;
        if (!cancelled) setLoading(false);
      }
      if (cancelled) return;
      timer = window.setTimeout(tick, hasActive ? FAST_POLL_MS : SLOW_POLL_MS);
    }

    tick();
    const onVis = () => {
      if (!document.hidden && Date.now() - lastFetch.current > FAST_POLL_MS) {
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
    // Dependency list intentionally empty: we don't want the effect
    // to re-run on every render. The cadence decision uses the freshly
    // fetched response, never component state, so nothing here goes
    // stale.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    return data.grabs.filter((g) => {
      if (filter !== "any" && g.status !== filter) return false;
      if (!needle) return true;
      return (
        g.release_title.toLowerCase().includes(needle) ||
        (g.performer_name || "").toLowerCase().includes(needle) ||
        (g.release_indexer || "").toLowerCase().includes(needle)
      );
    });
  }, [data, filter, q]);

  // Group the filtered grabs into render items: a scene with 2+ attempts
  // becomes a single SceneGroup card; everything else (lone grabs, packs,
  // and grabs with no scene id) stays a standalone row. This is what makes
  // "how many torrents am I trying for this scene" legible — every retry /
  // pick-another for one scene shares its stashdb id, so they collapse into
  // one card. Order follows the first attempt's position in `filtered`, so
  // the existing newest-first ordering is preserved.
  const items = useMemo<GrabListItem[]>(
    () => groupGrabsByScene(filtered),
    [filtered],
  );

  if (loading) return <div className="empty">Loading grabs…</div>;
  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!data) return null;

  const totals = data.totals || {};
  const anyTotal = Object.values(totals).reduce((s, n) => s + (n || 0), 0);

  return (
    <div>
      {/* Toolbar: identity (title + stats) on the left, search +
          live count on the right — one row that uses the width. */}
      <div className="grab-toolbar">
        <div className="grab-toolbar-id">
          <h2>Grabs</h2>
          <span className="grab-toolbar-stats">
            {anyTotal} total ·{" "}
            {(ACTIVE_STATUSES as GrabStatus[]).reduce(
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
            placeholder="Filter by title, performer, indexer…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
          <span className="grab-toolbar-count">
            {filtered.length}/{data.grabs.length}
          </span>
        </div>
      </div>

      {addOpen && (
        <div className="grab-add-form">
          <input
            ref={fileRef}
            type="file"
            accept=".torrent"
            onChange={onPickTorrent}
          />
          {addInspecting && (
            <span className="grab-add-hint">Reading torrent…</span>
          )}
          {addInspect && (
            <div className="torrent-inspect">
              <div className="ti-summary">
                <span className={"ti-kind " + addInspect.kind}>
                  {addInspect.kind === "pack" ? "PACK" : "SINGLE"}
                </span>
                <strong className="ti-name">
                  {addInspect.name || "(unnamed torrent)"}
                </strong>
                <span className="ti-meta">
                  {addInspect.video_count} video
                  {addInspect.video_count === 1 ? "" : "s"} ·{" "}
                  {humanSize(addInspect.total_size, "?")}
                </span>
              </div>
              {addInspect.suggested_performers.length > 0 ? (
                <div className="ti-suggest">
                  <span className="muted">Folder:</span>
                  {addInspect.suggested_performers.map((p) => (
                    <button
                      key={p.stash_id}
                      type="button"
                      className={
                        "ti-chip" + (addName === p.name ? " sel" : "")
                      }
                      onClick={() => setAddName(p.name)}
                      title={`${p.scene_count} scenes in library`}
                    >
                      {p.favorite ? "★ " : ""}
                      {p.name}
                    </button>
                  ))}
                </div>
              ) : (
                <div className="ti-suggest muted">
                  No library performer detected in the name — set a folder
                  below.
                </div>
              )}
            </div>
          )}
          <input
            type="text"
            placeholder="folder — default (manual)"
            value={addName}
            onChange={(e) => setAddName(e.target.value)}
          />
          <button
            className="grab-add-go"
            onClick={submitTorrent}
            disabled={addBusy || addInspecting || !addFile}
          >
            {addBusy ? "Adding…" : "Add"}
          </button>
          {(addFile || addName) && (
            <button
              type="button"
              className="grab-add-clear"
              onClick={resetAddForm}
              disabled={addBusy}
              title="Clear — deselect the torrent and clear the folder"
            >
              ✕ Clear
            </button>
          )}
          {addErr && <span className="grab-add-err">{addErr}</span>}
          <span className="grab-add-hint">
            forage downloads it, then places into{" "}
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
        <button
          className={"grab-chip chip-any" + (filter === "any" ? " active" : "")}
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
          {/* Force-adopt torrents added to the client manually, right now. */}
          <button
            className="grab-adopt-btn"
            onClick={scanForDownloads}
            disabled={adopting}
            title="Adopt torrents you added to the download client manually — skips the 5-minute wait"
          >
            {adopting ? "Scanning…" : "↻ Scan for downloads"}
          </button>
        </span>
      </div>

      {notice && (
        <div className="grab-notice" onClick={() => setNotice(null)}>
          {notice}
          <span className="grab-notice-x">×</span>
        </div>
      )}

      {filtered.length === 0 ? (
        <div className="empty">No grabs match this filter.</div>
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
  status: GrabStatus;
  count: number;
  active: boolean;
  onClick: () => void;
  dot?: boolean;
}) {
  return (
    <button
      className={"grab-chip chip-" + status + (active ? " active" : "")}
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
      terminal = { label: "Confirmed", at: confirmedAt, done: true, tone: "confirmed" };
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
    case "scanned":
      terminal = { label: "Identifying", at: 0, done: false, active: true };
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
    return (
      <div className="grab-match-hero mismatch">
        <svg
          className="grab-match-glyph"
          viewBox="0 0 40 40"
          aria-hidden="true"
        >
          <circle className="ring" cx="20" cy="20" r="17" />
          <path className="x1" d="M14 14 L26 26" />
          <path className="x2" d="M26 14 L14 26" />
        </svg>
        <div className="grab-match-hero-body">
          <div className="grab-match-hero-title">Different scene</div>
          <div className="grab-match-hero-sub grab-match-diff">
            <span className="grab-match-leg">
              <span className="grab-match-leg-k">predicted</span>
              <a href={stashdbScene(predicted)} target="_blank" rel="noopener noreferrer">
                {predicted}
              </a>
              {conf && <span className="grab-match-badge dim">{conf}</span>}
            </span>
            <span className="grab-match-rarr">→</span>
            <span className="grab-match-leg">
              <span className="grab-match-leg-k">stash got</span>
              <a href={stashdbScene(actual)} target="_blank" rel="noopener noreferrer">
                {actual}
              </a>
            </span>
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

// liveOutcome buckets a status into the three the summary line counts:
// "live" (still in flight), "done" (landed in the library), or "dead"
// (failed / abandoned / didn't match). Mirrors IN_FLIGHT / OUTCOME.
function attemptTally(grabs: Grab[]): { live: number; done: number; dead: number } {
  let live = 0;
  let done = 0;
  let dead = 0;
  for (const g of grabs) {
    if (g.status === "confirmed") done++;
    else if (isActiveStatus(g.status)) live++;
    else dead++; // mismatched / orphaned / failed
  }
  return { live, done, dead };
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
  const tally = attemptTally(group.grabs);
  const n = group.grabs.length;
  const parts: string[] = [];
  if (tally.live) parts.push(`${tally.live} in flight`);
  if (tally.done) parts.push(`${tally.done} in library`);
  if (tally.dead) parts.push(`${tally.dead} dead`);
  return (
    <li className="grab-scene-group">
      <div className="gsg-head">
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
      <ul className="gsg-attempts">{children}</ul>
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
          onError={(e) => {
            (e.currentTarget as HTMLImageElement).style.display = "none";
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

// DupCard is one scene the pack duplicated. "yours" is the pre-existing
// library copy (the highest-resolution one when several exist); "pack" is
// what this download delivered. Keep yours drops the pack's copy; Keep pack
// drops the original(s); Keep both leaves everything and dismisses the item.
function DupCard({
  dup,
  busy,
  disabled,
  onResolve,
}: {
  dup: DuplicateReview;
  busy: boolean;
  disabled: boolean;
  onResolve: (keep: "existing" | "pack" | "both") => void;
}) {
  // Representative existing copy = highest resolution, so "yours" reflects the
  // best file the user already holds.
  const best = [...dup.existing].sort(
    (a, b) => (b.height ?? 0) - (a.height ?? 0),
  )[0];
  const others = dup.existing.length - 1;
  // "Keep yours" destroys the pack's copy and "Keep pack" destroys your
  // original(s) — both irreversible (file deleted server-side), so they use
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
        <DupCopyRow tag="yours" copy={best} />
        <DupCopyRow tag="pack" copy={dup.pack} />
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
          {busy
            ? "…"
            : armed === "existing"
              ? "Delete pack copy?"
              : "Keep yours"}
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
            : "Keep pack"}
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
  const [deleting, setDeleting] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const [matchVal, setMatchVal] = useState("");
  const [matchBusy, setMatchBusy] = useState(false);
  const [matchErr, setMatchErr] = useState<string | null>(null);
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
      onResolvedDuplicate(
        keep === "both"
          ? "Kept both copies"
          : keep === "pack"
            ? "Kept the pack copy, removed the original"
            : "Kept your copy, removed the pack's",
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
      onMatched(); // parent refreshes; row re-renders as confirmed
    } catch (e) {
      setMatchErr((e as Error).message);
      setMatchBusy(false);
    }
  }

  // Offer manual StashDB matching when phash didn't cleanly link the
  // file: a wrong-scene mismatch, or anything placed that never got a
  // StashDB cross-id (e.g. "in library (scanned)" — no fingerprint on
  // StashDB). Needs the file on disk to apply onto.
  const canMatch =
    !!g.placed_path && (g.status === "mismatched" || !g.actual_stashdb_id);

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
    // Two-step: first click arms, second click within 4s commits.
    if (!confirmDelete) {
      setConfirmDelete(true);
      confirmTimer.current = window.setTimeout(
        () => setConfirmDelete(false),
        4000,
      );
      return;
    }
    if (confirmTimer.current) clearTimeout(confirmTimer.current);
    setConfirmDelete(false);
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
    <li className={"grab-row status-" + g.status + (expanded ? " open" : "")}>
      <button className="grab-row-head" onClick={onToggle}>
        <div className="grab-row-badges">
          <span className={"grab-status-badge chip-" + g.status}>{g.status}</span>
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
              title="This pack delivered scenes you already have — open to choose which copy to keep"
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
        <span className="grab-caret">{expanded ? "▼" : "▶"}</span>
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
                  <span className={"grab-poster-badge chip-" + g.status}>
                    {g.status}
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
                    — scenes this pack delivered that you already have
                  </span>
                </div>
                {dups.map((d) => (
                  <DupCard
                    key={d.id}
                    dup={d}
                    busy={dupBusy === d.id}
                    disabled={dupBusy != null}
                    onResolve={(keep) => resolveDup(d.id, keep)}
                  />
                ))}
                {dupErr && <div className="grab-delete-err">{dupErr}</div>}
              </div>
            );
          })()}

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
                  : "Not on StashDB by fingerprint — match it manually:"}
              </span>
              <div className="grab-match-tool-row">
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

          {/* Action bar */}
          {/* Set / change the performer folder — re-files the (still-
              seeding) download into <library>/<performer>/. The fix for an
              Unsorted or mis-identified grab. Packs included: forage files a
              whole pack into one performer folder, so it's reassignable too. */}
          <div className="grab-setperf">
              <span className="grab-setperf-label">
                {g.performer_name && g.performer_name !== "Unsorted"
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
                again; Pick another release is the fix there). */}
            {g.status === "failed" && (
              <button
                className="grab-action retry"
                onClick={handleRetry}
                disabled={retrying}
                title="Re-attempt this release"
              >
                {retrying ? "Retrying…" : "Retry ↻"}
              </button>
            )}
            {retryErr && <span className="grab-delete-err">{retryErr}</span>}
            {/* When a grab stalled or didn't land cleanly, jump back to the
                scene's releases to pick a different one — forage never
                auto-retries. */}
            {g.predicted_stashdb_id &&
              (g.stalled ||
                g.status === "failed" ||
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
        </div>
      )}
    </li>
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
