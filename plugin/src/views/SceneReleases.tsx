import { useEffect, useState } from "react";
import {
  fetchSceneReleases,
  postGrab,
  type SceneRelease,
  type SceneReleasesResponse,
} from "../api";

// Map the matcher's 0..1 confidence into one of five outline tiers.
// Only applied to verified releases — the unverified section already
// stands apart visually.
function confTier(c: number): string {
  if (c >= 0.70) return "conf-1";
  if (c >= 0.55) return "conf-2";
  if (c >= 0.40) return "conf-3";
  if (c >= 0.25) return "conf-4";
  return "conf-5";
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

type GrabState =
  | { status: "idle" }
  | { status: "grabbing" }
  | { status: "queued"; client: string }
  | { status: "error"; message: string };

export default function SceneReleases({
  sceneId,
  performerName,
}: {
  sceneId: string;
  // Set by the parent route — whichever performer page the user
  // navigated from. Threaded into /grab so forage places the file
  // under <library_root>/<performer>/.
  performerName?: string;
}) {
  const [data, setData] = useState<SceneReleasesResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Per-release grab state, keyed by download_url.
  const [grabs, setGrabs] = useState<Record<string, GrabState>>({});
  const [showUnverified, setShowUnverified] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setData(null);
    setError(null);
    fetchSceneReleases(sceneId)
      .then((r) => {
        if (cancelled) return;
        setData(r);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(e.message);
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [sceneId]);

  if (loading) return <div className="empty">Searching for releases…</div>;
  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!data) return null;

  const verified = data.releases.filter((r) => r.verified);
  const unverified = data.releases.filter((r) => !r.verified);

  const grab = async (rel: SceneRelease) => {
    setGrabs((g) => ({ ...g, [rel.download_url]: { status: "grabbing" } }));
    try {
      const res = await postGrab({
        download_url: rel.download_url,
        release_title: rel.title,
        release_size: rel.size,
        release_indexer: rel.indexer,
        protocol: rel.protocol,
        scene_id: data.scene.stashdb_id,
        confidence: rel.confidence,
        performer_name: performerName,
      });
      setGrabs((g) => ({
        ...g,
        [rel.download_url]: { status: "queued", client: res.client || "?" },
      }));
    } catch (e) {
      setGrabs((g) => ({
        ...g,
        [rel.download_url]: {
          status: "error",
          message: (e as Error).message,
        },
      }));
    }
  };

  return (
    <div>
      <div className="scene-hero">
        {data.scene.image_url && (
          <img
            className="scene-hero-img"
            src={data.scene.image_url}
            alt=""
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.display = "none";
            }}
          />
        )}
        <div className="scene-hero-info">
          <h2>{data.scene.title || "(untitled)"}</h2>
          <div className="meta">
            {data.scene.date && <span>{data.scene.date}</span>}
            {data.scene.date && data.scene.studio && <span> · </span>}
            {data.scene.studio && <span>{data.scene.studio}</span>}
          </div>
          {data.scene.performers.length > 0 && (
            <div className="meta">
              {data.scene.performers.map((p) => p.name).join(", ")}
            </div>
          )}
          <div className="meta">
            <a
              href={`https://stashdb.org/scenes/${data.scene.stashdb_id}`}
              target="_blank"
              rel="noopener noreferrer"
            >
              View on StashDB ↗
            </a>
          </div>
        </div>
      </div>

      {verified.length === 0 && unverified.length === 0 ? (
        <div className="empty">
          No releases found for this scene.
        </div>
      ) : (
        <>
          {verified.length > 0 && (
            <section>
              <h3 className="section-header">Verified ({verified.length})</h3>
              <ReleaseList releases={verified} grabs={grabs} onGrab={grab} />
            </section>
          )}
          {unverified.length > 0 && (
            <section>
              <h3
                className="section-header collapsible"
                onClick={() => setShowUnverified((v) => !v)}
              >
                {showUnverified ? "▼" : "▶"} Unverified ({unverified.length}) — different
                scenes that share a title token
              </h3>
              {showUnverified && (
                <ReleaseList releases={unverified} grabs={grabs} onGrab={grab} />
              )}
            </section>
          )}
        </>
      )}
    </div>
  );
}

function ReleaseList({
  releases,
  grabs,
  onGrab,
}: {
  releases: SceneRelease[];
  grabs: Record<string, GrabState>;
  onGrab: (r: SceneRelease) => void;
}) {
  return (
    <ul className="release-list">
      {releases.map((r) => {
        const state = grabs[r.download_url] || { status: "idle" };
        const tier = r.verified && r.confidence > 0 ? confTier(r.confidence) : "";
        return (
          <li key={r.download_url} className={"release" + (tier ? " " + tier : "")}>
            <div className="release-body">
              <div className="release-title">{r.title}</div>
              <div className="release-meta">
                <span>{r.indexer}</span>
                <span>·</span>
                <span>{r.protocol}</span>
                <span>·</span>
                <span>{humanSize(r.size)}</span>
                <span>·</span>
                <span>
                  {r.protocol === "usenet"
                    ? `${r.grabs} grabs`
                    : `${r.seeders} seeders`}
                </span>
                {r.confidence > 0 && (
                  <>
                    <span>·</span>
                    <span>conf {r.confidence.toFixed(2)}</span>
                  </>
                )}
              </div>
            </div>
            <GrabButton state={state} onClick={() => onGrab(r)} />
          </li>
        );
      })}
    </ul>
  );
}

function GrabButton({ state, onClick }: { state: GrabState; onClick: () => void }) {
  switch (state.status) {
    case "idle":
      return (
        <button className="grab-btn" onClick={onClick}>
          Grab ↓
        </button>
      );
    case "grabbing":
      return (
        <button className="grab-btn" disabled>
          Queueing…
        </button>
      );
    case "queued":
      return (
        <button className="grab-btn queued" disabled>
          Queued → {state.client}
        </button>
      );
    case "error":
      return (
        <button
          className="grab-btn error"
          onClick={onClick}
          title={state.message}
        >
          Failed — retry
        </button>
      );
  }
}
