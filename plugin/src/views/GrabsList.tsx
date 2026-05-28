import {
  Fragment,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  ACTIVE_STATUSES,
  deleteGrab,
  DeleteGrabResult,
  fetchGrabDetail,
  fetchGrabs,
  Grab,
  GrabDetail,
  GrabsResponse,
  GrabStatus,
  isActiveStatus,
} from "../api";

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

export default function GrabsList() {
  const [data, setData] = useState<GrabsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<GrabStatus | "any">("any");
  const [q, setQ] = useState("");
  const [expanded, setExpanded] = useState<Set<number>>(new Set());
  const [notice, setNotice] = useState<string | null>(null);
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

    async function tick() {
      if (cancelled) return;
      if (document.hidden) {
        // Don't waste cycles when the tab isn't visible. Reschedule
        // and try again later — visibilitychange handler also kicks.
        timer = window.setTimeout(tick, SLOW_POLL_MS);
        return;
      }
      try {
        const r = await fetchGrabs({ limit: 200 });
        if (cancelled) return;
        setData(r);
        setError(null);
        lastFetch.current = Date.now();
      } catch (e) {
        if (cancelled) return;
        setError((e as Error).message);
      } finally {
        if (!cancelled) setLoading(false);
      }
      if (cancelled) return;
      const hasActive = (data?.grabs || []).some((g) =>
        isActiveStatus(g.status),
      );
      timer = window.setTimeout(tick, hasActive ? FAST_POLL_MS : SLOW_POLL_MS);
    }

    tick();
    const onVis = () => {
      if (!document.hidden && Date.now() - lastFetch.current > FAST_POLL_MS) {
        // Tab refocused after a hidden period — refetch immediately.
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
    // to re-run on every render. Latest data flows in via the closure
    // capture above (the active-poll decision reads `data` at call time).
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
          {filtered.map((g) => (
            <GrabRow
              key={g.id}
              g={g}
              expanded={expanded.has(g.id)}
              onToggle={() =>
                setExpanded((s) => {
                  const next = new Set(s);
                  if (next.has(g.id)) next.delete(g.id);
                  else next.add(g.id);
                  return next;
                })
              }
              onDeleted={(res) => handleDeleted(g.id, res)}
            />
          ))}
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

function GrabRow({
  g,
  expanded,
  onToggle,
  onDeleted,
}: {
  g: Grab;
  expanded: boolean;
  onToggle: () => void;
  onDeleted: (res: DeleteGrabResult) => void;
}) {
  const [detail, setDetail] = useState<GrabDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteErr, setDeleteErr] = useState<string | null>(null);
  const confirmTimer = useRef<number | undefined>(undefined);
  const fetchedDetail = useRef(false);

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
        <span className={"grab-status-badge chip-" + g.status}>{g.status}</span>
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
            <div className="grab-poster">
              {detail?.image_url ? (
                <img
                  src={detail.image_url}
                  alt=""
                  loading="lazy"
                  onError={(e) => {
                    (e.currentTarget as HTMLImageElement).style.visibility =
                      "hidden";
                  }}
                />
              ) : (
                <div className="grab-poster-empty">
                  {detailLoading ? "" : "no preview"}
                </div>
              )}
              <span className={"grab-poster-badge chip-" + g.status}>
                {g.status}
              </span>
            </div>

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

          {/* Action bar */}
          <div className="grab-actions">
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

function humanSize(b: number): string {
  if (!b) return "";
  const units = ["B", "K", "M", "G", "T"];
  let i = 0;
  let v = b;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return v.toFixed(1) + units[i] + "B";
}

function relativeTime(unix: number): string {
  if (!unix) return "";
  const ageSec = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (ageSec < 60) return `${ageSec}s ago`;
  if (ageSec < 3600) return `${Math.floor(ageSec / 60)}m ago`;
  if (ageSec < 86_400) return `${Math.floor(ageSec / 3600)}h ago`;
  return `${Math.floor(ageSec / 86_400)}d ago`;
}
