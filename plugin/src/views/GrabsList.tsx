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
      <div className="page-header">
        <h2>Grabs</h2>
        <div className="meta">
          {anyTotal} total ·{" "}
          {(ACTIVE_STATUSES as GrabStatus[]).reduce(
            (s, k) => s + (totals[k] || 0),
            0,
          )}{" "}
          in flight
        </div>
      </div>

      <div className="grab-filter">
        <button
          className={"grab-chip chip-any" + (filter === "any" ? " active" : "")}
          onClick={() => setFilter("any")}
        >
          <span className="chip-label">all</span>
          <span className="chip-count">{anyTotal}</span>
        </button>

        <div className="grab-flow">
          <div className="grab-flow-group">
            <span className="grab-flow-label">In flight</span>
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
          </div>

          <div className="grab-flow-group">
            <span className="grab-flow-label">Outcome</span>
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
        </div>
      </div>

      <div className="controls">
        <input
          type="text"
          placeholder="Filter by title, performer, indexer…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <span className="count">
          {filtered.length} / {data.grabs.length}
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

  // Fetch the rich detail (scene thumbnail/title/performers + local
  // Stash link) the first time the row opens. Cheap to keep cached
  // for the row's lifetime.
  useEffect(() => {
    if (!expanded || detail || detailLoading) return;
    let cancelled = false;
    setDetailLoading(true);
    fetchGrabDetail(g.id)
      .then((d) => {
        if (!cancelled) setDetail(d);
      })
      .catch(() => {
        /* card just renders without the hero */
      })
      .finally(() => {
        if (!cancelled) setDetailLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [expanded, detail, detailLoading, g.id]);

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
          {/* Scene hero — thumbnail + title + performer chips. Only
              once the detail fetch resolves to a real scene. */}
          {detail && (detail.title || detail.image_url) && (
            <div className="grab-scene">
              {detail.image_url && (
                <div className="grab-scene-thumb">
                  <img
                    src={detail.image_url}
                    alt=""
                    loading="lazy"
                    onError={(e) => {
                      (e.currentTarget as HTMLImageElement).style.display =
                        "none";
                    }}
                  />
                </div>
              )}
              <div className="grab-scene-info">
                <div className="grab-scene-title">
                  {detail.title || "(untitled scene)"}
                </div>
                {(detail.date || detail.studio) && (
                  <div className="grab-scene-meta">
                    {detail.date && <span>{detail.date}</span>}
                    {detail.date && detail.studio && (
                      <span className="sep">·</span>
                    )}
                    {detail.studio && <span>{detail.studio}</span>}
                  </div>
                )}
                {detail.performers.length > 0 && (
                  <div className="grab-perf-chips">
                    {detail.performers.map((p, i) => (
                      <span className="grab-perf-chip" key={i}>
                        {p.name}
                      </span>
                    ))}
                  </div>
                )}
              </div>
            </div>
          )}
          {detailLoading && !detail && (
            <div className="grab-scene-loading">Loading scene…</div>
          )}

          {g.reason && <DetailRow label="Reason">{g.reason}</DetailRow>}
          {g.place_error && (
            <DetailRow label="Place error" className="err">
              {g.place_error}
            </DetailRow>
          )}
          {g.placed_path && (
            <DetailRow label="Placed path">
              <code>{g.placed_path}</code>
            </DetailRow>
          )}
          {g.client_name && (
            <DetailRow label="Client file">
              <code>{g.client_name}</code>
            </DetailRow>
          )}
          {g.predicted_stashdb_id && (
            <DetailRow label="Predicted scene">
              <a
                href={`https://stashdb.org/scenes/${g.predicted_stashdb_id}`}
                target="_blank"
                rel="noopener noreferrer"
              >
                {g.predicted_stashdb_id}
              </a>
              {g.predicted_confidence != null &&
                g.predicted_confidence > 0 && (
                  <span className="muted">
                    {" "}
                    (match {g.predicted_confidence.toFixed(2)})
                  </span>
                )}
            </DetailRow>
          )}
          {g.actual_stashdb_id &&
            g.actual_stashdb_id !== g.predicted_stashdb_id && (
              <DetailRow label="Actual scene" className="warn">
                <a
                  href={`https://stashdb.org/scenes/${g.actual_stashdb_id}`}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  {g.actual_stashdb_id}
                </a>{" "}
                <span className="muted">(differs from predicted)</span>
              </DetailRow>
            )}
          <DetailRow label="Timeline">
            <span>grabbed {relativeTime(g.grabbed_at)}</span>
            {g.completed_at && g.completed_at > 0 && (
              <>
                <span className="sep">·</span>
                <span>completed {relativeTime(g.completed_at)}</span>
              </>
            )}
            {g.placed_at && g.placed_at > 0 && (
              <>
                <span className="sep">·</span>
                <span>placed {relativeTime(g.placed_at)}</span>
              </>
            )}
            {g.confirmed_at && g.confirmed_at > 0 && (
              <>
                <span className="sep">·</span>
                <span>confirmed {relativeTime(g.confirmed_at)}</span>
              </>
            )}
          </DetailRow>

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

function DetailRow({
  label,
  className,
  children,
}: {
  label: string;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={"detail-row" + (className ? " " + className : "")}>
      <span className="detail-label">{label}</span>
      <span className="detail-value">{children}</span>
    </div>
  );
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
