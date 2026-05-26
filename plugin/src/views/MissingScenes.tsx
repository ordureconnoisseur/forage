import { useEffect, useState } from "react";
import { fetchMissing, type MissingResponse, type MissingScene } from "../api";

export default function MissingScenes({
  performerId,
  onPickScene,
}: {
  performerId: string;
  // Receives the performer name too so the scene-releases page can
  // pass it through to /grab — the placer needs to know which library
  // folder to drop the file in.
  onPickScene: (stashDBID: string, performerName: string) => void;
}) {
  const [data, setData] = useState<MissingResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setData(null);
    setError(null);
    fetchMissing(performerId)
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
  }, [performerId]);

  if (loading) return <div className="empty">Loading missing scenes…</div>;
  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!data) return null;

  return (
    <div>
      <div className="page-header">
        <h2>{data.performer.name}</h2>
        <div className="meta">
          {data.total_scenes} on StashDB · {data.owned_count} in library ·{" "}
          <strong>{data.missing.length} missing</strong>
        </div>
      </div>
      {data.missing.length === 0 ? (
        <div className="empty">
          You have every StashDB scene for this performer in your library.
        </div>
      ) : (
        <div className="scene-grid">
          {data.missing.map((s) => (
            <SceneCard
              key={s.stashdb_id}
              s={s}
              onPick={() => onPickScene(s.stashdb_id, data.performer.name)}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function SceneCard({ s, onPick }: { s: MissingScene; onPick: () => void }) {
  return (
    <button className="scene-card" onClick={onPick}>
      <div className="scene-thumb">
        {s.image_url ? (
          <img
            src={s.image_url}
            alt=""
            loading="lazy"
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.display = "none";
            }}
          />
        ) : null}
      </div>
      <div className="scene-info">
        <div className="title">{s.title || "(untitled)"}</div>
        <div className="meta">
          {s.date && <span>{s.date}</span>}
          {s.date && s.studio && <span> · </span>}
          {s.studio && <span>{s.studio}</span>}
        </div>
        {s.performers && s.performers.length > 0 ? (
          <div className="performers">
            {s.performers
              .map((p) => (p.as && p.as !== p.name ? `${p.name} (as ${p.as})` : p.name))
              .join(", ")}
          </div>
        ) : null}
      </div>
    </button>
  );
}
