import { useEffect, useState } from "react";
import { CheckIcon } from "../icons";
import { fetchPacks, postGrab, type Pack } from "../api";
import { humanSize } from "../format";

// Whole-performer pack torrents — a one-click alternative to grabbing scenes
// individually. Lives on the performer page (was previously inside the
// now-removed collection view). Self-contained: its own on-demand search and
// per-pack grab state. The search hits indexers and takes ~10s+, so nothing
// runs until the user clicks; switching performers reverts to the prompt.

type GrabState = "idle" | "queued" | "error";

// packKey is a stable per-pack identity for grab state. download_url can be
// empty for an indexer that supplies neither a download URL nor a magnet, so
// fall back to info_url/title — otherwise such packs share one state bucket
// (grabbing one paints them all queued).
function packKey(p: Pack): string {
  return p.download_url || p.info_url || p.title;
}

// Packs above this size get a size-emphasis class so the user doesn't grab
// 190GB unaware.
const PACK_HUGE_BYTES = 80 * 1024 * 1024 * 1024;

export default function PerformerPacks({
  performerId,
  performerName,
}: {
  performerId: string;
  performerName: string;
}) {
  const [requested, setRequested] = useState(false);
  const [packs, setPacks] = useState<Pack[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [grabState, setGrabState] = useState<Record<string, GrabState>>({});

  // Reset to the click-to-search prompt when the performer changes.
  useEffect(() => {
    setRequested(false);
    setPacks(null);
    setError(null);
    setGrabState({});
  }, [performerId]);

  useEffect(() => {
    if (!requested) return;
    const ctrl = new AbortController();
    setPacks(null);
    setError(null);
    fetchPacks(performerId, ctrl.signal)
      .then((r) => setPacks(r.packs))
      .catch((e) => {
        if (ctrl.signal.aborted) return;
        setError((e as Error).message);
      });
    return () => ctrl.abort();
  }, [requested, performerId]);

  async function grab(p: Pack) {
    if (grabState[packKey(p)] === "queued") return;
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
      setGrabState((s) => ({ ...s, [packKey(p)]: "queued" }));
    } catch {
      setGrabState((s) => ({ ...s, [packKey(p)]: "error" }));
    }
  }

  if (!requested) {
    return (
      <div className="packs-section">
        <div className="packs-head">Packs</div>
        <div className="packs-note muted">
          Look for whole-performer pack torrents (a one-shot alternative to
          grabbing scenes one by one).
        </div>
        <button className="packs-search-btn" onClick={() => setRequested(true)}>
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
        <button className="packs-search-btn" onClick={() => setRequested(true)}>
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
        <button className="packs-search-btn" onClick={() => setRequested(true)}>
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
        Whole-performer collections: grabbing one downloads every scene, then
        forage removes any you already own.
      </div>
      <ul className="packs-list">
        {packs.map((p) => {
          const state = grabState[packKey(p)] || "idle";
          const queued = state === "queued";
          const huge = p.size >= PACK_HUGE_BYTES;
          return (
            <li key={packKey(p)} className={"pack-card" + (queued ? " queued" : "")}>
              <div className="pack-main">
                <code className="pack-title">{p.title}</code>
                <div className="pack-meta">
                  <span className="pack-indexer">{p.indexer}</span>
                  <span className="sep">·</span>
                  <span className={"pack-size" + (huge ? " huge" : "")}>
                    {humanSize(p.size, "?")}
                  </span>
                  {p.video_count > 0 && (
                    <>
                      <span className="sep">·</span>
                      <span className="pack-vids">~{p.video_count} videos</span>
                    </>
                  )}
                  <span className="sep">·</span>
                  <span>{p.seeders} seeders</span>
                </div>
              </div>
              <button
                className="pack-grab"
                disabled={queued}
                onClick={() => grab(p)}
                title={
                  huge ? `Large download, ${humanSize(p.size, "?")}` : undefined
                }
              >
                {queued ? (
          <>
            <CheckIcon size={10} /> queued
          </>
        ) : state === "error" ? (
          "retry"
        ) : (
          "Grab pack"
        )}
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
