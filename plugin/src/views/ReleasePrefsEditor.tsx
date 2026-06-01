import { ReactNode, useState } from "react";
import {
  compileRules,
  parsePrefs,
  ProtocolPref,
  ReleasePrefs,
  Resolution,
  ResolutionFloor,
} from "../releasePrefs";
import type { IndexerInfo } from "../api";

// ReleasePrefsEditor — the friendly, NO-TYPING release-ranking UI. The user
// expresses preference by ORDERING (drag, or ↑/↓ for keyboard) and simple
// toggles; we compile that into the daemon's scoring.Rule[] and hand BOTH
// the prefs JSON and the compiled rules JSON back to the parent to persist.
// The scoring engine never changes — this is purely a rule generator.

const RES_META: Record<Resolution, { label: string; cls: string }> = {
  "4k": { label: "4K / 2160p", cls: "res-4k" },
  "1080p": { label: "1080p", cls: "res-1080" },
  "720p": { label: "720p", cls: "res-720" },
  "480p": { label: "480p / SD", cls: "res-480" },
};

function arrayMove<T>(arr: T[], from: number, to: number): T[] {
  const next = [...arr];
  const [x] = next.splice(from, 1);
  next.splice(to, 0, x);
  return next;
}

// XIcon — a plain stroke "✕", used for block/unblock controls instead of an
// emoji (emoji render inconsistently across platforms and clash with the
// app's flat icon language).
function XIcon() {
  return (
    <svg
      viewBox="0 0 24 24"
      width="1em"
      height="1em"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <path d="M6 6 18 18 M18 6 6 18" />
    </svg>
  );
}

// ReorderList is a tiny dependency-free drag-to-reorder list: HTML5
// draggable + onDragOver live-swap, plus ↑/↓ buttons so it's fully
// keyboard-accessible (mirrors the advanced editor's move affordance).
function ReorderList<T>({
  items,
  getKey,
  renderItem,
  onReorder,
}: {
  items: T[];
  getKey: (item: T) => string;
  renderItem: (item: T, i: number) => ReactNode;
  onReorder: (from: number, to: number) => void;
}) {
  const [dragIdx, setDragIdx] = useState<number | null>(null);
  return (
    <div className="reorder-list">
      {items.map((item, i) => (
        <div
          key={getKey(item)}
          className={"reorder-row" + (dragIdx === i ? " dragging" : "")}
          draggable
          onDragStart={() => setDragIdx(i)}
          onDragEnd={() => setDragIdx(null)}
          onDragOver={(e) => {
            e.preventDefault();
            if (dragIdx !== null && dragIdx !== i) {
              onReorder(dragIdx, i);
              setDragIdx(i);
            }
          }}
        >
          <span className="reorder-grip" aria-hidden>
            ⠿
          </span>
          <span className="reorder-rank">{i + 1}</span>
          {renderItem(item, i)}
          <span className="reorder-move">
            <button
              type="button"
              onClick={() => i > 0 && onReorder(i, i - 1)}
              disabled={i === 0}
              title="Move up"
              aria-label="Move up"
            >
              ↑
            </button>
            <button
              type="button"
              onClick={() => i < items.length - 1 && onReorder(i, i + 1)}
              disabled={i === items.length - 1}
              title="Move down"
              aria-label="Move down"
            >
              ↓
            </button>
          </span>
        </div>
      ))}
    </div>
  );
}

export default function ReleasePrefsEditor({
  value,
  indexers,
  onChange,
}: {
  // The releasePrefs JSON string from config ("" = friendly defaults).
  value: string;
  // The user's Prowlarr indexers (empty when Prowlarr is unconfigured —
  // the indexer section then hides itself).
  indexers: IndexerInfo[];
  // Called with (prefsJSON, compiledRulesJSON) on any change. The parent
  // writes BOTH into the config patch.
  onChange: (prefsJSON: string, rulesJSON: string) => void;
}) {
  const prefs = parsePrefs(value);

  // emit serialises the new prefs AND their compiled rules so the daemon
  // keeps scoring off releaseRules while we own releasePrefs.
  const emit = (next: ReleasePrefs) =>
    onChange(JSON.stringify(next), JSON.stringify(compileRules(next)));

  // ── Resolution ────────────────────────────────────────────────────────
  const reorderRes = (from: number, to: number) =>
    emit({ ...prefs, resolutionOrder: arrayMove(prefs.resolutionOrder, from, to) });

  // ── Indexers (reconcile stored order against live Prowlarr list) ────────
  const blockedSet = new Set(prefs.blockedIndexers);
  const existing = indexers.map((i) => i.name);
  const existingSet = new Set(existing);
  const metaByName = new Map(indexers.map((i) => [i.name, i]));

  const ranked: string[] = [];
  for (const n of prefs.indexerOrder) {
    if (existingSet.has(n) && !blockedSet.has(n) && !ranked.includes(n)) ranked.push(n);
  }
  for (const n of existing) {
    if (!blockedSet.has(n) && !ranked.includes(n)) ranked.push(n);
  }
  const blocked = existing.filter((n) => blockedSet.has(n));

  const reorderIdx = (from: number, to: number) =>
    emit({ ...prefs, indexerOrder: arrayMove(ranked, from, to) });
  const blockIdx = (name: string) =>
    emit({
      ...prefs,
      indexerOrder: ranked.filter((n) => n !== name),
      blockedIndexers: [...prefs.blockedIndexers.filter((n) => n !== name), name],
    });
  const unblockIdx = (name: string) =>
    emit({
      ...prefs,
      blockedIndexers: prefs.blockedIndexers.filter((n) => n !== name),
      indexerOrder: [...ranked, name],
    });

  const protoBadge = (name: string) => {
    const proto = metaByName.get(name)?.protocol ?? "";
    if (!proto) return null;
    return <span className={"idx-proto idx-proto-" + proto}>{proto}</span>;
  };

  return (
    <div className="prefs-editor">
      <p className="settings-tip">
        No scores, no regex — just rank what you prefer and forage figures out
        the points. Resolution always wins; indexer and protocol break ties
        within the same resolution. Need the raw rules? Flip to{" "}
        <b>Advanced</b>.
      </p>

      {/* ── Resolution ─────────────────────────────────────────────── */}
      <h4 className="prefs-head">Resolution</h4>
      <p className="prefs-sub">Drag to rank — your top choice grabbed first.</p>
      <ReorderList
        items={prefs.resolutionOrder}
        getKey={(r) => r}
        onReorder={reorderRes}
        renderItem={(r) => (
          <span className="prefs-row-main">
            <span className={"res-badge " + RES_META[r].cls}>
              {RES_META[r].label}
            </span>
          </span>
        )}
      />
      <label className="prefs-floor">
        <span>Don't grab below</span>
        <select
          value={prefs.resolutionFloor}
          onChange={(e) =>
            emit({ ...prefs, resolutionFloor: e.target.value as ResolutionFloor })
          }
        >
          <option value="">No limit</option>
          <option value="480p">480p</option>
          <option value="720p">720p</option>
          <option value="1080p">1080p</option>
        </select>
      </label>

      {/* ── Indexers ───────────────────────────────────────────────── */}
      {indexers.length > 0 ? (
        <>
          <h4 className="prefs-head">Indexers</h4>
          <p className="prefs-sub">
            Drag to rank your preferred sources. Block one to never grab from
            it.
          </p>
          {ranked.length > 0 && (
            <ReorderList
              items={ranked}
              getKey={(n) => n}
              onReorder={reorderIdx}
              renderItem={(n) => (
                <span className="prefs-row-main">
                  <span className="idx-name">{n}</span>
                  {protoBadge(n)}
                  <button
                    type="button"
                    className="idx-block"
                    onClick={() => blockIdx(n)}
                    title="Block this indexer"
                    aria-label={"Block " + n}
                  >
                    <XIcon />
                  </button>
                </span>
              )}
            />
          )}
          {blocked.length > 0 && (
            <div className="prefs-blocked">
              <span className="prefs-blocked-label">Blocked</span>
              {blocked.map((n) => (
                <span className="idx-blocked-chip" key={n}>
                  {n}
                  <button
                    type="button"
                    onClick={() => unblockIdx(n)}
                    title="Unblock"
                    aria-label={"Unblock " + n}
                  >
                    <XIcon />
                  </button>
                </span>
              ))}
            </div>
          )}
        </>
      ) : (
        <p className="prefs-sub prefs-noindexers">
          Configure Prowlarr to rank or block individual indexers.
        </p>
      )}

      {/* ── Protocol ───────────────────────────────────────────────── */}
      <h4 className="prefs-head">Protocol</h4>
      <p className="prefs-sub">
        Tie-breaker only — never overrides a better resolution.
      </p>
      <div className="prefs-seg" role="group" aria-label="Protocol preference">
        {(
          [
            ["torrent", "Prefer torrents"],
            ["none", "No preference"],
            ["usenet", "Prefer usenet"],
          ] as [ProtocolPref, string][]
        ).map(([val, label]) => (
          <button
            type="button"
            key={val}
            className={"prefs-seg-btn" + (prefs.protocolPref === val ? " active" : "")}
            aria-pressed={prefs.protocolPref === val}
            onClick={() => emit({ ...prefs, protocolPref: val })}
          >
            {label}
          </button>
        ))}
      </div>
    </div>
  );
}
