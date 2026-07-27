import { useCallback, useEffect, useState } from "react";
import {
  DestructionEntry,
  fetchDestructions,
  restoreDestruction,
} from "../api";
import { humanSize } from "../format";

// DeletionsList renders the destruction journal — everything forage
// destroyed, trashed, refused or restored, newest first, with the complete
// file list snapshotted at decision time. "trashed" entries carry a Restore
// button: the undo window the trash system exists for, surfaced instead of
// living behind curl.

const OUTCOME_LABEL: Record<string, string> = {
  intent: "in flight",
  destroyed: "deleted",
  trashed: "in trash",
  refused: "refused",
  failed: "failed",
  restored: "restored",
};

export default function DeletionsList() {
  const [entries, setEntries] = useState<DestructionEntry[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<number | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(() => {
    fetchDestructions(200)
      .then((r) => {
        setEntries(r);
        setErr(null);
      })
      .catch((e) => setErr((e as Error).message));
  }, []);
  useEffect(load, [load]);

  async function restore(e: DestructionEntry) {
    setBusy(e.id);
    setNotice(null);
    try {
      await restoreDestruction(e.id);
      setNotice(
        `Restored ${e.files.length} file${e.files.length === 1 ? "" : "s"} — Stash will re-index shortly`,
      );
      load();
    } catch (er) {
      setNotice("Restore failed: " + (er as Error).message);
    } finally {
      setBusy(null);
    }
  }

  if (err) return <div className="empty error">Failed to load: {err}</div>;
  if (entries === null) return <div className="empty">Loading journal…</div>;

  return (
    <div>
      <div className="grab-toolbar">
        <div className="grab-toolbar-id">
          <h2>Deletions</h2>
          <span className="grab-toolbar-stats">
            every deletion forage performed or refused · trashed items are
            restorable until the retention sweep
          </span>
        </div>
        <div className="grab-toolbar-search">
          <button className="grab-adopt-btn" onClick={load}>
            ↻ Refresh
          </button>
        </div>
      </div>

      {notice && (
        <div className="grab-notice" onClick={() => setNotice(null)}>
          {notice}
          <span className="grab-notice-x">×</span>
        </div>
      )}

      {entries.length === 0 ? (
        <div className="empty">
          Nothing yet — forage hasn&rsquo;t deleted anything since the journal
          began.
        </div>
      ) : (
        <ul className="del-list">
          {entries.map((e) => (
            <li className={"del-row outcome-" + e.outcome} key={e.id}>
              <span className={"del-badge outcome-" + e.outcome}>
                {OUTCOME_LABEL[e.outcome] ?? e.outcome}
              </span>
              <div className="del-body">
                <div className="del-title">
                  {e.title || e.files[0]?.path || "(no file recorded)"}
                  <span className="del-reason">{e.reason}</span>
                </div>
                <div className="del-meta">
                  {e.files.slice(0, 2).map((f) => (
                    <code className="del-path" key={f.path} title={f.path}>
                      {f.path}
                      {f.size ? ` (${humanSize(f.size)})` : ""}
                    </code>
                  ))}
                  {e.files.length > 2 && (
                    <span className="del-more">
                      +{e.files.length - 2} more file
                      {e.files.length - 2 === 1 ? "" : "s"}
                    </span>
                  )}
                  <span className="del-time">{relTime(e.created_at)}</span>
                </div>
                {e.outcome === "refused" && e.detail && (
                  <div className="del-detail">{e.detail}</div>
                )}
              </div>
              {e.outcome === "trashed" && (
                <button
                  className="grab-action retry"
                  disabled={busy !== null}
                  onClick={() => restore(e)}
                  title="Move the file(s) back where they came from and re-index"
                >
                  {busy === e.id ? "Restoring…" : "Restore"}
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function relTime(unix: number): string {
  if (!unix) return "";
  const age = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (age < 60) return `${age}s ago`;
  if (age < 3600) return `${Math.floor(age / 60)}m ago`;
  if (age < 86_400) return `${Math.floor(age / 3600)}h ago`;
  return `${Math.floor(age / 86_400)}d ago`;
}
