import { useEffect, useState } from "react";
import { fetchMissing, type MissingScene } from "../api";

// CollectionMode — "complete the collection" for one performer. P1
// skeleton: loads the performer's missing scenes and lists them (the
// set that will be searched). The parallel search + selectable
// candidates land in later phases.
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

  useEffect(() => {
    let cancelled = false;
    setScenes(null);
    setError(null);
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

  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!scenes) return <div className="empty">Loading missing scenes…</div>;

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
          <span className="coll-sub">{scenes.length} missing scenes</span>
        </div>
        <button className="coll-grab" disabled>
          Grab 0 selected
        </button>
      </div>

      {scenes.length === 0 ? (
        <div className="empty">Nothing missing — collection complete.</div>
      ) : (
        <ul className="coll-list">
          {scenes.map((s) => (
            <li key={s.stashdb_id} className="coll-row">
              <div className="coll-thumb">
                {s.image_url ? (
                  <img
                    src={s.image_url}
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
                <div className="coll-row-title">{s.title || "(untitled)"}</div>
                <div className="coll-row-meta">
                  {s.date && <span>{s.date}</span>}
                  {s.date && s.studio && <span className="sep">·</span>}
                  {s.studio && <span>{s.studio}</span>}
                </div>
              </div>
              <div className="coll-row-status">queued for search…</div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
