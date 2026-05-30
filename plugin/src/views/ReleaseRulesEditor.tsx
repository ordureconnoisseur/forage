import { useMemo } from "react";
import type { ReleaseRule } from "../api";

// The daemon's built-in defaults, mirrored here so the editor can show
// them as the starting point and offer a "reset to defaults" action.
// Keep in sync with scoring.DefaultRules() on the server.
const DEFAULT_RULES: ReleaseRule[] = [
  { label: "x265 / HEVC", pattern: "\\b(x265|hevc|h\\.?265)\\b", points: 100 },
  { label: "AV1", pattern: "\\bav1\\b", points: 90 },
  { label: "1080p", pattern: "\\b1080p?\\b", points: 80 },
  { label: "4K / 2160p", pattern: "\\b(2160p?|4k|uhd)\\b", points: 60 },
  { label: "720p", pattern: "\\b720p?\\b", points: 20 },
  { label: "x264 / H.264", pattern: "\\b(x264|h\\.?264|avc)\\b", points: 10 },
  { label: "480p / SD", pattern: "\\b(480p?|360p?|sd)\\b", points: -50 },
  {
    label: "CAM / TS / screener",
    pattern: "\\b(cam|ts|telesync|telecine|screener|scr)\\b",
    points: 0,
    reject: true,
  },
];

function parseRules(json: string): ReleaseRule[] {
  if (!json.trim()) return DEFAULT_RULES;
  try {
    const r = JSON.parse(json);
    if (Array.isArray(r) && r.length > 0) return r as ReleaseRule[];
  } catch {
    /* fall through to defaults */
  }
  return DEFAULT_RULES;
}

// ReleaseRulesEditor edits the release-scoring preference list. Rules are
// a flat list: each match adds its points to a release's score; the
// highest-scoring verified release is preferred. A "reject" rule hard-
// excludes any release it matches. Serialises back to a JSON string for
// the config patch.
export default function ReleaseRulesEditor({
  value,
  onChange,
}: {
  // The releaseRules JSON string from config ("" = defaults).
  value: string;
  // Called with the new JSON string whenever a rule changes.
  onChange: (json: string) => void;
}) {
  const rules = useMemo(() => parseRules(value), [value]);

  const emit = (next: ReleaseRule[]) => onChange(JSON.stringify(next));

  const update = (i: number, patch: Partial<ReleaseRule>) => {
    const next = rules.map((r, k) => (k === i ? { ...r, ...patch } : r));
    emit(next);
  };
  const remove = (i: number) => emit(rules.filter((_, k) => k !== i));
  const move = (i: number, dir: -1 | 1) => {
    const j = i + dir;
    if (j < 0 || j >= rules.length) return;
    const next = [...rules];
    [next[i], next[j]] = [next[j], next[i]];
    emit(next);
  };
  const add = () =>
    emit([...rules, { label: "New rule", pattern: "", points: 0 }]);
  const resetDefaults = () => emit(DEFAULT_RULES);

  return (
    <div className="rules-editor">
      <p className="settings-tip">
        Releases are ranked by SCORE: each rule that matches a release title
        adds its points (negative to penalise). The highest-scoring verified
        release is preferred; ties break by seeders/grabs. A <b>reject</b> rule
        hard-excludes any release it matches (e.g. CAM). Order is cosmetic —
        scoring is additive, not tiered.
      </p>
      <div className="rules-list">
        <div className="rule-row rule-head">
          <span>Label</span>
          <span>Pattern (regex)</span>
          <span>Points</span>
          <span>Reject</span>
          <span />
        </div>
        {rules.map((r, i) => (
          <div className="rule-row" key={i}>
            <input
              className="rule-label"
              value={r.label}
              onChange={(e) => update(i, { label: e.target.value })}
              placeholder="name"
            />
            <input
              className="rule-pattern"
              value={r.pattern}
              spellCheck={false}
              onChange={(e) => update(i, { pattern: e.target.value })}
              placeholder="\\b1080p\\b"
            />
            <input
              className="rule-points"
              type="number"
              value={r.points}
              onChange={(e) =>
                update(i, { points: parseInt(e.target.value || "0", 10) })
              }
            />
            <input
              type="checkbox"
              checked={!!r.reject}
              onChange={(e) => update(i, { reject: e.target.checked })}
              title="Hard-exclude any release matching this rule"
            />
            <span className="rule-actions">
              <button onClick={() => move(i, -1)} title="Move up" disabled={i === 0}>
                ↑
              </button>
              <button
                onClick={() => move(i, 1)}
                title="Move down"
                disabled={i === rules.length - 1}
              >
                ↓
              </button>
              <button
                className="rule-del"
                onClick={() => remove(i)}
                title="Remove rule"
              >
                ✕
              </button>
            </span>
          </div>
        ))}
      </div>
      <div className="rules-actions">
        <button className="rules-add" onClick={add}>
          + Add rule
        </button>
        <button className="rules-reset" onClick={resetDefaults}>
          Reset to defaults
        </button>
      </div>
    </div>
  );
}
