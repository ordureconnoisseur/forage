import { useEffect, useState } from "react";
import {
  fetchMissing,
  fetchPacks,
  fetchSceneReleases,
  postGrab,
  type MissingScene,
  type Pack,
  type SceneRelease,
} from "../api";
import { ResBadge } from "../ResBadge";

const SEARCH_CONCURRENCY = 4;
const GRAB_CONCURRENCY = 3;
// Only pre-tick a scene when its best verified release clears this
// confidence. The verifier flags any release whose top candidate is
// the target scene, which includes near-zero coincidental title-token
// matches (e.g. "Oil Overload" 0.03) — those must NOT auto-select.
// Below the floor the scene waits for manual review.
const AUTO_PICK_FLOOR = 0.5;

type RowStatus = "pending" | "searching" | "done" | "empty" | "error";
type GrabState = "idle" | "queued" | "error";

interface RowState {
  status: RowStatus;
  releases: SceneRelease[];
  // download_url of the chosen release, or null = nothing picked.
  pickedURL: string | null;
  // grab lifecycle for the picked release, once the user fires it.
  grab: GrabState;
  error?: string;
}

function blankRow(): RowState {
  return { status: "pending", releases: [], pickedURL: null, grab: "idle" };
}

// CollectionMode — "complete the collection" for one performer. P2:
// fans a bounded-concurrency Prowlarr search out over every missing
// scene, renders each row as it resolves, and auto-picks the top
// verified release. Selecting + bulk-grab land in P3/P4.
export default function CollectionMode({
  performerId,
  onBack,
}: {
  performerId: string;
  onBack: (performerId: string) => void;
}) {
  const [scenes, setScenes] = useState<MissingScene[] | null>(null);
  const [performerName, setPerformerName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [rows, setRows] = useState<Record<string, RowState>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [grabbing, setGrabbing] = useState(false);

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
    if (packGrab[p.download_url] === "queued") return;
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
      setPackGrab((s) => ({ ...s, [p.download_url]: "queued" }));
    } catch {
      setPackGrab((s) => ({ ...s, [p.download_url]: "error" }));
    }
  }

  // Load the target set.
  useEffect(() => {
    let cancelled = false;
    setScenes(null);
    setError(null);
    setRows({});
    fetchMissing(performerId)
      .then((r) => {
        if (cancelled) return;
        setPerformerName(r.performer.name);
        setScenes(r.missing);
      })
      .catch((e) => {
        if (cancelled) return;
        setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, [performerId]);

  // Fan the search out once the target set is known. A shared
  // AbortController cancels every in-flight request when the page
  // unmounts — leaving stops the scan (and stops hitting Prowlarr).
  useEffect(() => {
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
        setRow(scene.stashdb_id, { status: "searching" });
        try {
          const res = await fetchSceneReleases(scene.stashdb_id, ctrl.signal);
          if (cancelled) return;
          // Rank by confidence so the strongest match leads (the
          // endpoint sorts verified-first/popularity, which floats
          // junk coincidental "verified" matches above the real one).
          const releases = (res.releases || [])
            .slice()
            .sort((a, b) => b.confidence - a.confidence);
          const best = releases.find((x) => x.verified);
          setRow(scene.stashdb_id, {
            status: releases.length === 0 ? "empty" : "done",
            releases,
            pickedURL:
              best && best.confidence >= AUTO_PICK_FLOOR
                ? best.download_url
                : null,
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
  }, [scenes]);

  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!scenes) return <div className="empty">Loading missing scenes…</div>;

  const searched = scenes.filter((s) => {
    const st = rows[s.stashdb_id]?.status;
    return st === "done" || st === "empty" || st === "error";
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
        if (!row || row.grab === "queued" || !row.pickedURL) return null;
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
          <h2>Complete collection</h2>
          <span className="coll-sub">
            {scanning
              ? `searching ${searched}/${total}…`
              : `${total} scenes · ${selectedCount} selected`}
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
        <button
          className="coll-grab"
          disabled={selectedCount === 0 || grabbing}
          onClick={bulkGrab}
        >
          {grabbing ? "Grabbing…" : `Grab ${selectedCount} selected`}
        </button>
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
            key={p.download_url || p.info_url}
            pack={p}
            state={grabState[p.download_url] || "idle"}
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
            {humanSize(pack.size)}
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
        title={huge ? `Large download — ${humanSize(pack.size)}` : undefined}
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
}: {
  scene: MissingScene;
  row: RowState;
  expanded: boolean;
  onToggle: () => void;
  onExpand: () => void;
  onPick: (downloadURL: string) => void;
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
                <span className="coll-state err">search failed</span>
              )}
              {row.status === "empty" && (
                <span className="coll-state muted">no releases found</span>
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
                </div>
              )}
              {row.status === "done" && !picked && (
                <span className="coll-state warn">no confident match</span>
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
                  {rel.indexer} · {rel.protocol} · {humanSize(rel.size)} ·{" "}
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

function humanSize(b: number): string {
  if (!b) return "?";
  const units = ["B", "K", "M", "G", "T"];
  let i = 0;
  let v = b;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return v.toFixed(1) + units[i] + "B";
}
