import { useEffect, useState } from "react";
import {
  fetchCollectionJob,
  fetchMissing,
  fetchPacks,
  fetchSceneReleases,
  grabJobScene,
  postGrab,
  type MissingScene,
  type Pack,
  type SceneRelease,
} from "../api";
import { ResBadge } from "../ResBadge";
import { humanSize } from "../format";

// Keep this low: each scene's release search fans out a couple of
// Prowlarr queries, and Prowlarr (→ the trackers, esp. PornoLab) chokes
// on too many concurrent searches — context-deadline "search failed"
// across the board. 2 scenes × ~2 lean queries is a sane ceiling.
const SEARCH_CONCURRENCY = 2;
const GRAB_CONCURRENCY = 3;
// Only pre-tick a scene when its best verified release clears this
// confidence. The verifier flags any release whose top candidate is
// the target scene, which includes near-zero coincidental title-token
// matches (e.g. "Oil Overload" 0.03) — those must NOT auto-select.
// Below the floor the scene waits for manual review.
const AUTO_PICK_FLOOR = 0.5;

type RowStatus =
  | "pending"
  | "searching"
  | "done"
  | "empty"
  | "error"
  | "inflight"; // already grabbed/downloading — not re-grabbable
type GrabState = "idle" | "queued" | "error";

interface RowState {
  status: RowStatus;
  releases: SceneRelease[];
  // download_url of the chosen release, or null = nothing picked.
  pickedURL: string | null;
  // grab lifecycle for the picked release, once the user fires it.
  grab: GrabState;
  error?: string;
  // For status==="inflight": the existing grab's status (downloading…).
  grabStatus?: string;
  // True when auto-pick selected a release for this row on search. Lets
  // the UI distinguish "auto-pick found a confident match that the user
  // then UNCHECKED" (→ "none selected") from "auto-pick found nothing
  // confident among the results" (→ offer a deep search).
  autoPicked?: boolean;
}

function blankRow(): RowState {
  return { status: "pending", releases: [], pickedURL: null, grab: "idle" };
}

// sceneStatusFromJob maps a server job's scene status to a row status for
// the rare hydrated case where a scene has no stored candidates (e.g. it
// errored or wasn't reached before cancel).
function sceneStatusFromJob(st: string): RowStatus {
  switch (st) {
    case "no_result":
      return "empty";
    case "error":
      return "error";
    case "skipped":
      return "inflight";
    case "found":
    case "grabbed":
    case "no_match":
    default:
      return "done";
  }
}

// CollectionMode — "complete the collection" for one performer. P2:
// fans a bounded-concurrency Prowlarr search out over every missing
// scene, renders each row as it resolves, and auto-picks the top
// verified release. Selecting + bulk-grab land in P3/P4.
export default function CollectionMode({
  performerId,
  onBack,
  onRunOnServer,
  sceneIds,
  jobId,
}: {
  performerId: string;
  onBack: (performerId: string) => void;
  // Hand the crawl to the daemon (survives leaving the page) and jump to
  // the Jobs tab. Passed the optional scene subset.
  onRunOnServer: (performerId: string, sceneIds?: string[]) => void;
  // When set, scope the collection to only these StashDB scene ids (the
  // user's multi-select from MissingScenes) instead of every missing
  // scene. undefined = full collection.
  sceneIds?: string[];
  // When set, HYDRATE from a finished server job instead of crawling:
  // load /jobs/{id}, build rows from its stored candidate lists, and route
  // grabs to the job's re-grab endpoint. Turns the Jobs "review" action
  // into the identical interactive view, backed by server state.
  jobId?: string;
}) {
  const [scenes, setScenes] = useState<MissingScene[] | null>(null);
  const [performerName, setPerformerName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [rows, setRows] = useState<Record<string, RowState>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [grabbing, setGrabbing] = useState(false);
  // Count of missing scenes hidden because they're already being grabbed.
  const [inflightHidden, setInflightHidden] = useState(0);

  // Packs are searched on demand (it hits indexers and takes ~10s+), not
  // automatically on page open. packsFor holds the performer the user
  // asked to search; the fetch runs only when it matches the current
  // performer, so switching performers reverts to the click-to-search
  // prompt instead of carrying over a stale result.
  const [packs, setPacks] = useState<Pack[] | null>(null);
  const [packErr, setPackErr] = useState<string | null>(null);
  const [packGrab, setPackGrab] = useState<Record<string, GrabState>>({});
  const [packsFor, setPacksFor] = useState<string | null>(null);
  const packsRequested = packsFor === performerId;

  useEffect(() => {
    if (packsFor !== performerId) return; // not requested for this performer
    const ctrl = new AbortController();
    setPacks(null);
    setPackErr(null);
    fetchPacks(performerId, ctrl.signal)
      .then((r) => setPacks(r.packs))
      .catch((e) => {
        if (ctrl.signal.aborted) return;
        setPackErr((e as Error).message);
      });
    return () => ctrl.abort();
  }, [packsFor, performerId]);

  function startPackSearch() {
    setPackGrab({});
    setPacksFor(performerId);
  }

  async function grabPack(p: Pack) {
    if (packGrab[packKey(p)] === "queued") return;
    try {
      await postGrab({
        download_url: p.download_url,
        release_title: p.title,
        release_size: p.size,
        release_indexer: p.indexer,
        protocol: p.protocol,
        performer_name: performerName,
        kind: "pack",
        video_count: p.video_count,
      });
      setPackGrab((s) => ({ ...s, [packKey(p)]: "queued" }));
    } catch {
      setPackGrab((s) => ({ ...s, [packKey(p)]: "error" }));
    }
  }

  // Load the target set — either hydrate from a finished server job
  // (jobId set) or fetch the performer's missing scenes to crawl.
  useEffect(() => {
    let cancelled = false;
    setScenes(null);
    setError(null);
    setRows({});

    if (jobId) {
      // Hydrate from the job: rows come pre-populated with the server's
      // stored candidates + picks; no client-side crawl.
      fetchCollectionJob(jobId)
        .then((job) => {
          if (cancelled) return;
          setPerformerName(job.performer_name);
          const ms: MissingScene[] = [];
          const hydrated: Record<string, RowState> = {};
          for (const sc of job.scenes) {
            ms.push({ stashdb_id: sc.stashdb_id, title: sc.title, performers: [] });
            const releases = sc.candidates || [];
            hydrated[sc.stashdb_id] = {
              status: releases.length > 0 ? "done" : sceneStatusFromJob(sc.status),
              releases,
              pickedURL: sc.picked_url || null,
              autoPicked: !!sc.picked_url,
              grab: sc.status === "grabbed" ? "queued" : "idle",
            };
          }
          setScenes(ms);
          setRows(hydrated);
        })
        .catch((e) => {
          if (cancelled) return;
          setError((e as Error).message);
        });
      return () => {
        cancelled = true;
      };
    }

    fetchMissing(performerId)
      .then((r) => {
        if (cancelled) return;
        setPerformerName(r.performer.name);
        // Scope to the hand-picked subset when one was passed; otherwise
        // the full missing set.
        let target =
          sceneIds && sceneIds.length > 0
            ? r.missing.filter((s) => new Set(sceneIds).has(s.stashdb_id))
            : r.missing;
        // Hide scenes already being grabbed (downloading from an earlier
        // session). They aren't actionable here, and leaving them in made
        // them look re-grabbable until the search worker reached them and
        // flipped them to "already grabbing". Count the hidden ones so the
        // header can note them.
        const before = target.length;
        target = target.filter((s) => !s.grab_status);
        setInflightHidden(before - target.length);
        setScenes(target);
      })
      .catch((e) => {
        if (cancelled) return;
        setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
    // scopeKey (stable join of the scene-id subset) so re-scoping reloads.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [performerId, jobId, (sceneIds || []).join(",")]);

  // Fan the search out once the target set is known. A shared
  // AbortController cancels every in-flight request when the page
  // unmounts — leaving stops the scan (and stops hitting Prowlarr).
  useEffect(() => {
    if (jobId) return; // hydrated from the server; no client-side crawl
    if (!scenes || scenes.length === 0) return;
    const ctrl = new AbortController();
    let cancelled = false;
    const queue = [...scenes];

    const setRow = (id: string, patch: Partial<RowState>) =>
      setRows((r) => ({ ...r, [id]: { ...(r[id] || blankRow()), ...patch } }));

    async function worker() {
      while (!cancelled) {
        const scene = queue.shift();
        if (!scene) return;
        // Already grabbed / downloading: don't re-search or auto-select
        // it — skip straight to an "in flight" row so it can't be
        // re-grabbed (and we don't waste an indexer search on it).
        if (scene.grab_status) {
          setRow(scene.stashdb_id, {
            status: "inflight",
            grabStatus: scene.grab_status,
          });
          continue;
        }
        setRow(scene.stashdb_id, { status: "searching" });
        try {
          const res = await fetchSceneReleases(scene.stashdb_id, {
            performer: performerName,
            lean: true, // collection fan-out: few queries/scene, don't flood Prowlarr
            signal: ctrl.signal,
          });
          if (cancelled) return;
          // Rank by confidence so the strongest match leads (the
          // endpoint sorts verified-first/popularity, which floats
          // junk coincidental "verified" matches above the real one).
          const releases = (res.releases || [])
            .slice()
            .sort((a, b) => b.confidence - a.confidence);
          const best = releases.find((x) => x.verified);
          const autoPick =
            best && best.confidence >= AUTO_PICK_FLOOR ? best.download_url : null;
          setRow(scene.stashdb_id, {
            status: releases.length === 0 ? "empty" : "done",
            releases,
            pickedURL: autoPick,
            autoPicked: autoPick != null,
          });
        } catch (e) {
          if (cancelled || ctrl.signal.aborted) return;
          setRow(scene.stashdb_id, {
            status: "error",
            error: (e as Error).message,
          });
        }
      }
    }

    void Promise.allSettled(
      Array.from({ length: SEARCH_CONCURRENCY }, () => worker()),
    );
    return () => {
      cancelled = true;
      ctrl.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scenes]);

  // Re-run one scene's release search, deep (lean=false → full
   // multi-spelling query set) — for a row that came up empty / no
   // confident match / failed under the lean collection search.
  const retryScene = async (scene: MissingScene) => {
    setRows((r) => ({
      ...r,
      [scene.stashdb_id]: {
        ...(r[scene.stashdb_id] || blankRow()),
        status: "searching",
      },
    }));
    try {
      const res = await fetchSceneReleases(scene.stashdb_id, {
        performer: performerName,
        lean: false, // deep: every performer spelling × studio/year + title
      });
      const releases = (res.releases || [])
        .slice()
        .sort((a, b) => b.confidence - a.confidence);
      const best = releases.find((x) => x.verified);
      const autoPick =
        best && best.confidence >= AUTO_PICK_FLOOR ? best.download_url : null;
      setRows((r) => ({
        ...r,
        [scene.stashdb_id]: {
          ...(r[scene.stashdb_id] || blankRow()),
          status: releases.length === 0 ? "empty" : "done",
          releases,
          pickedURL: autoPick,
          autoPicked: autoPick != null,
        },
      }));
    } catch (e) {
      setRows((r) => ({
        ...r,
        [scene.stashdb_id]: {
          ...(r[scene.stashdb_id] || blankRow()),
          status: "error",
          error: (e as Error).message,
        },
      }));
    }
  };

  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!scenes) return <div className="empty">Loading missing scenes…</div>;

  const searched = scenes.filter((s) => {
    const st = rows[s.stashdb_id]?.status;
    // inflight is a settled state (we skipped its search) — count it so
    // the progress bar can complete.
    return st === "done" || st === "empty" || st === "error" || st === "inflight";
  }).length;
  const total = scenes.length;
  const selectedCount = scenes.filter((s) => {
    const row = rows[s.stashdb_id];
    return row?.pickedURL && row.grab !== "queued";
  }).length;
  const queuedCount = scenes.filter(
    (s) => rows[s.stashdb_id]?.grab === "queued",
  ).length;
  const scanning = searched < total;

  // Fire a grab for every picked-but-not-yet-queued scene, bounded.
  // Rows flip to "queued" in place as each returns; failures stay
  // selectable for a retry.
  async function bulkGrab() {
    if (!scenes || grabbing) return;
    const targets = scenes
      .map((s) => {
        const row = rows[s.stashdb_id];
        // Never grab an already-in-flight scene (downloading from a prior
        // session) or one already queued this session.
        if (
          !row ||
          row.status === "inflight" ||
          row.grab === "queued" ||
          !row.pickedURL
        )
          return null;
        const rel = row.releases.find((r) => r.download_url === row.pickedURL);
        return rel ? { scene: s, rel } : null;
      })
      .filter((t): t is { scene: MissingScene; rel: SceneRelease } => !!t);
    if (targets.length === 0) return;

    setGrabbing(true);
    const queue = [...targets];
    const setRow = (id: string, patch: Partial<RowState>) =>
      setRows((r) => ({ ...r, [id]: { ...(r[id] || blankRow()), ...patch } }));

    async function worker() {
      for (;;) {
        const t = queue.shift();
        if (!t) return;
        try {
          if (jobId) {
            // Hydrated from a server job: grab through the job's re-grab
            // endpoint so the daemon updates the job's own state.
            await grabJobScene(jobId, t.scene.stashdb_id, t.rel.download_url);
          } else {
            await postGrab({
              download_url: t.rel.download_url,
              release_title: t.rel.title,
              release_size: t.rel.size,
              release_indexer: t.rel.indexer,
              protocol: t.rel.protocol,
              scene_id: t.scene.stashdb_id,
              confidence: t.rel.confidence,
              performer_name: performerName,
            });
          }
          setRow(t.scene.stashdb_id, { grab: "queued" });
        } catch {
          setRow(t.scene.stashdb_id, { grab: "error" });
        }
      }
    }

    await Promise.allSettled(
      Array.from({ length: GRAB_CONCURRENCY }, () => worker()),
    );
    setGrabbing(false);
  }

  const toggle = (s: MissingScene) =>
    setRows((r) => {
      const row = r[s.stashdb_id] || blankRow();
      if (row.status === "inflight") return r; // already grabbing — not selectable
      const next =
        row.pickedURL != null
          ? null
          : (row.releases.find((x) => x.verified)?.download_url ??
            row.releases[0]?.download_url ??
            null);
      return { ...r, [s.stashdb_id]: { ...row, pickedURL: next } };
    });

  // Choose a specific release for a scene (from the expanded
  // candidate list). Selecting always ticks the row.
  const pick = (id: string, url: string) =>
    setRows((r) => ({
      ...r,
      [id]: { ...(r[id] || blankRow()), pickedURL: url },
    }));

  const toggleExpand = (id: string) =>
    setExpanded((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });

  // Bulk (de)selection over non-queued rows.
  const bulkSet = (fn: (row: RowState) => string | null) =>
    setRows((r) => {
      if (!scenes) return r;
      const next = { ...r };
      for (const s of scenes) {
        const row = next[s.stashdb_id];
        if (!row || row.grab === "queued") continue;
        next[s.stashdb_id] = { ...row, pickedURL: fn(row) };
      }
      return next;
    });
  const selectAllVerified = () =>
    bulkSet((row) => row.releases.find((x) => x.verified)?.download_url ?? null);
  const clearAll = () => bulkSet(() => null);

  return (
    <div>
      <div className="coll-header">
        <div className="coll-head-id">
          <a
            href={`#/performer/${performerId}`}
            className="coll-back"
            onClick={(e) => {
              e.preventDefault();
              onBack(performerId);
            }}
          >
            ← {performerName || "performer"}
          </a>
          <h2>{sceneIds && sceneIds.length > 0 ? "Grab selected scenes" : "Complete collection"}</h2>
          <span className="coll-sub">
            {scanning
              ? `searching ${searched}/${total}…`
              : `${total} scenes · ${selectedCount} selected`}
            {inflightHidden > 0 && ` · ${inflightHidden} already grabbing (hidden)`}
            {queuedCount > 0 && ` · ${queuedCount} queued`}
          </span>
          {!scanning && total > 0 && (
            <div className="coll-bulk">
              <button onClick={selectAllVerified}>select verified</button>
              <span className="coll-bulk-sep">·</span>
              <button onClick={clearAll}>clear</button>
            </div>
          )}
        </div>
        <div className="coll-head-actions">
          <button
            className="coll-run-server"
            onClick={() => onRunOnServer(performerId, sceneIds)}
            title="Search every missing scene on the server in the background (keeps going if you close this page). Nothing is grabbed — you Review and pick from the Jobs tab."
          >
            Search on server →
          </button>
          <button
            className="coll-grab"
            disabled={selectedCount === 0 || grabbing}
            onClick={bulkGrab}
          >
            {grabbing ? "Grabbing…" : `Grab ${selectedCount} selected`}
          </button>
        </div>
      </div>

      <PacksSection
        requested={packsRequested}
        onSearch={startPackSearch}
        packs={packs}
        error={packErr}
        grabState={packGrab}
        onGrab={grabPack}
      />

      {scanning && (
        <div className="coll-progress">
          <div
            className="coll-progress-fill"
            style={{ width: `${total ? (searched / total) * 100 : 0}%` }}
          />
        </div>
      )}

      {scenes.length === 0 ? (
        <div className="empty">Nothing missing — collection complete.</div>
      ) : (
        <ul className="coll-list">
          {scenes.map((s) => (
            <CollectionRow
              key={s.stashdb_id}
              scene={s}
              row={rows[s.stashdb_id] || blankRow()}
              expanded={expanded.has(s.stashdb_id)}
              onToggle={() => toggle(s)}
              onExpand={() => toggleExpand(s.stashdb_id)}
              onPick={(url) => pick(s.stashdb_id, url)}
              onRetry={() => retryScene(s)}
            />
          ))}
        </ul>
      )}
    </div>
  );
}

// PacksSection lists whole-performer pack torrents at the top of the
// collection view — a one-click alternative to grabbing scenes
// individually. The search is on demand (it hits indexers and takes a
// while), so it starts as a button; nothing runs until the user clicks.
function PacksSection({
  requested,
  onSearch,
  packs,
  error,
  grabState,
  onGrab,
}: {
  requested: boolean;
  onSearch: () => void;
  packs: Pack[] | null;
  error: string | null;
  grabState: Record<string, GrabState>;
  onGrab: (p: Pack) => void;
}) {
  if (!requested) {
    return (
      <div className="packs-section">
        <div className="packs-head">Packs</div>
        <div className="packs-note muted">
          Look for whole-performer pack torrents (a one-shot alternative to
          grabbing scenes one by one).
        </div>
        <button className="packs-search-btn" onClick={onSearch}>
          Search for packs
        </button>
      </div>
    );
  }
  if (error) {
    return (
      <div className="packs-section">
        <div className="packs-head">Packs</div>
        <div className="packs-note err">pack search failed: {error}</div>
        <button className="packs-search-btn" onClick={onSearch}>
          Retry
        </button>
      </div>
    );
  }
  if (packs === null) {
    return (
      <div className="packs-section">
        <div className="packs-head">
          Packs <span className="coll-spinner" />
        </div>
        <div className="packs-note muted">searching indexers for packs…</div>
      </div>
    );
  }
  if (packs.length === 0) {
    return (
      <div className="packs-section">
        <div className="packs-head">Packs</div>
        <div className="packs-note muted">No packs found for this performer.</div>
        <button className="packs-search-btn" onClick={onSearch}>
          Search again
        </button>
      </div>
    );
  }

  return (
    <div className="packs-section">
      <div className="packs-head">
        Packs <span className="packs-count">{packs.length}</span>
      </div>
      <div className="packs-note muted">
        Whole-performer collections — grabbing one downloads every scene,
        then forage removes any you already own.
      </div>
      <ul className="packs-list">
        {packs.map((p) => (
          <PackCard
            key={packKey(p)}
            pack={p}
            state={grabState[packKey(p)] || "idle"}
            onGrab={() => onGrab(p)}
          />
        ))}
      </ul>
    </div>
  );
}

// huge: packs above this size get a size-emphasis class so the user
// doesn't grab 190GB unaware.
const PACK_HUGE_BYTES = 80 * 1024 * 1024 * 1024;

function PackCard({
  pack,
  state,
  onGrab,
}: {
  pack: Pack;
  state: GrabState;
  onGrab: () => void;
}) {
  const queued = state === "queued";
  const huge = pack.size >= PACK_HUGE_BYTES;
  return (
    <li className={"pack-card" + (queued ? " queued" : "")}>
      <div className="pack-main">
        <code className="pack-title">{pack.title}</code>
        <div className="pack-meta">
          <span className="pack-indexer">{pack.indexer}</span>
          <span className="sep">·</span>
          <span className={"pack-size" + (huge ? " huge" : "")}>
            {humanSize(pack.size, "?")}
          </span>
          {pack.video_count > 0 && (
            <>
              <span className="sep">·</span>
              <span className="pack-vids">~{pack.video_count} videos</span>
            </>
          )}
          <span className="sep">·</span>
          <span>{pack.seeders} seeders</span>
        </div>
      </div>
      <button
        className="pack-grab"
        disabled={queued}
        onClick={onGrab}
        title={huge ? `Large download — ${humanSize(pack.size, "?")}` : undefined}
      >
        {queued ? "queued ✓" : state === "error" ? "retry" : "Grab pack"}
      </button>
    </li>
  );
}

function CollectionRow({
  scene,
  row,
  expanded,
  onToggle,
  onExpand,
  onPick,
  onRetry,
}: {
  scene: MissingScene;
  row: RowState;
  expanded: boolean;
  onToggle: () => void;
  onExpand: () => void;
  onPick: (downloadURL: string) => void;
  onRetry: () => void;
}) {
  const picked = row.releases.find((r) => r.download_url === row.pickedURL);
  const selectable = row.status === "done";
  const canExpand = row.status === "done" && row.releases.length > 0;

  const queued = row.grab === "queued";

  return (
    <li
      className={
        "coll-row" +
        (row.pickedURL ? " picked" : "") +
        (queued ? " queued" : "")
      }
    >
      <div className="coll-row-head">
        <label className="coll-check">
          {queued ? (
            <span className="coll-queued-check" aria-label="queued">
              ✓
            </span>
          ) : (
            <input
              type="checkbox"
              checked={row.pickedURL != null}
              disabled={!selectable}
              onChange={onToggle}
            />
          )}
        </label>
        <div className="coll-thumb">
          {scene.image_url ? (
            <img
              src={scene.image_url}
              alt=""
              loading="lazy"
              onError={(e) => {
                (e.currentTarget as HTMLImageElement).style.visibility =
                  "hidden";
              }}
            />
          ) : null}
        </div>
        <div className="coll-row-main">
          <div className="coll-row-title">{scene.title || "(untitled)"}</div>
          <div className="coll-row-meta">
            {scene.date && <span>{scene.date}</span>}
            {scene.date && scene.studio && <span className="sep">·</span>}
            {scene.studio && <span>{scene.studio}</span>}
          </div>
        </div>
        <div className="coll-row-result">
          {queued ? (
            <span className="coll-state queued">queued ✓</span>
          ) : (
            <>
              {row.status === "pending" && (
                <span className="coll-state muted">queued…</span>
              )}
              {row.status === "searching" && (
                <span className="coll-state">
                  <span className="coll-spinner" /> searching…
                </span>
              )}
              {row.status === "error" && (
                <span className="coll-state err">
                  search failed
                  <button className="coll-retry" onClick={onRetry}>
                    ↻ retry
                  </button>
                </span>
              )}
              {row.status === "inflight" && (
                <span className="coll-state inflight">
                  ↓ already grabbing ({row.grabStatus || "in flight"})
                </span>
              )}
              {row.status === "empty" && (
                <span className="coll-state muted">
                  no releases found
                  <button className="coll-retry" onClick={onRetry}>
                    ↻ deep search
                  </button>
                </span>
              )}
              {row.status === "done" && picked && (
                <div className="coll-pick">
                  <span
                    className={
                      "coll-pick-tag " +
                      (picked.verified ? "verified" : "guess")
                    }
                  >
                    {picked.verified ? "verified" : "unverified"}{" "}
                    {picked.confidence.toFixed(2)}
                  </span>
                  <ResBadge title={picked.title} />
                  <code className="coll-pick-file">{picked.title}</code>
                  {row.grab === "error" && (
                    <span className="coll-grab-err">grab failed</span>
                  )}
                  <button
                    className="coll-retry"
                    onClick={onRetry}
                    title="Re-search this scene with the full (deep) query set"
                  >
                    ↻ deep
                  </button>
                </div>
              )}
              {row.status === "done" && !picked && row.autoPicked && (
                // Auto-pick selected something, then the user unchecked it.
                <span className="coll-state muted">
                  none selected
                  <button className="coll-retry" onClick={onExpand}>
                    {expanded ? "▾ pick one" : "▸ pick one"}
                  </button>
                  <button className="coll-retry" onClick={onRetry}>
                    ↻ deep search
                  </button>
                </span>
              )}
              {row.status === "done" && !picked && !row.autoPicked && (
                // Releases came back but none was a confident match.
                <span className="coll-state warn">
                  no confident match
                  <button className="coll-retry" onClick={onExpand}>
                    {expanded ? "▾ pick one" : `▸ ${row.releases.length}`}
                  </button>
                  <button className="coll-retry" onClick={onRetry}>
                    ↻ deep search
                  </button>
                </span>
              )}
            </>
          )}
        </div>
        {canExpand ? (
          <button
            className="coll-expand"
            onClick={onExpand}
            title="Show all candidates"
          >
            {expanded ? "▾" : "▸"}{" "}
            <span className="coll-expand-n">{row.releases.length}</span>
          </button>
        ) : (
          <span className="coll-expand placeholder" />
        )}
      </div>

      {expanded && canExpand && (
        <div className="coll-cands">
          {row.releases.map((rel) => (
            <label
              key={rel.download_url}
              className={
                "coll-cand" +
                (rel.download_url === row.pickedURL ? " sel" : "")
              }
            >
              <input
                type="radio"
                name={"pick-" + scene.stashdb_id}
                checked={rel.download_url === row.pickedURL}
                onChange={() => onPick(rel.download_url)}
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
                  {!rel.verified && rel.best_match_title && (
                    <span className="coll-cand-warn">
                      {" "}
                      · looks like {rel.best_match_title}
                    </span>
                  )}
                </span>
              </span>
            </label>
          ))}
        </div>
      )}
    </li>
  );
}

// packKey is a stable per-pack identity for grab state. download_url can
// be empty for an indexer that supplies neither a download URL nor a
// magnet, so fall back to info_url/title — otherwise such packs would
// share one state bucket (grabbing one paints them all queued).
function packKey(p: Pack): string {
  return p.download_url || p.info_url || p.title;
}
