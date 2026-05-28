import { useEffect, useState } from "react";
import {
  fetchMissing,
  fetchSceneReleases,
  type MissingScene,
  type SceneRelease,
} from "../api";

const SEARCH_CONCURRENCY = 4;

type RowStatus = "pending" | "searching" | "done" | "empty" | "error";

interface RowState {
  status: RowStatus;
  releases: SceneRelease[];
  // download_url of the chosen release, or null = nothing picked.
  pickedURL: string | null;
  error?: string;
}

function blankRow(): RowState {
  return { status: "pending", releases: [], pickedURL: null };
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
          const releases = res.releases || [];
          const topVerified = releases.find((x) => x.verified);
          setRow(scene.stashdb_id, {
            status: releases.length === 0 ? "empty" : "done",
            releases,
            pickedURL: topVerified ? topVerified.download_url : null,
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
  const selectedCount = scenes.filter(
    (s) => rows[s.stashdb_id]?.pickedURL,
  ).length;
  const scanning = searched < total;

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
          </span>
        </div>
        <button className="coll-grab" disabled={selectedCount === 0}>
          Grab {selectedCount} selected
        </button>
      </div>

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
              onToggle={() => toggle(s)}
            />
          ))}
        </ul>
      )}
    </div>
  );
}

function CollectionRow({
  scene,
  row,
  onToggle,
}: {
  scene: MissingScene;
  row: RowState;
  onToggle: () => void;
}) {
  const picked = row.releases.find((r) => r.download_url === row.pickedURL);
  const selectable = row.status === "done";
  const verifiedCount = row.releases.filter((r) => r.verified).length;

  return (
    <li className={"coll-row" + (row.pickedURL ? " picked" : "")}>
      <label className="coll-check">
        <input
          type="checkbox"
          checked={row.pickedURL != null}
          disabled={!selectable}
          onChange={onToggle}
        />
      </label>
      <div className="coll-thumb">
        {scene.image_url ? (
          <img
            src={scene.image_url}
            alt=""
            loading="lazy"
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.visibility = "hidden";
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
                "coll-pick-tag " + (picked.verified ? "verified" : "guess")
              }
            >
              {picked.verified
                ? `verified ${picked.confidence.toFixed(2)}`
                : `unverified ${picked.confidence.toFixed(2)}`}
            </span>
            <code className="coll-pick-file">{picked.title}</code>
          </div>
        )}
        {row.status === "done" && !picked && (
          <span className="coll-state warn">
            no confident match — {verifiedCount === 0 ? row.releases.length : 0}{" "}
            to review
          </span>
        )}
      </div>
    </li>
  );
}
