import { useMemo } from "react";
import type { ReleaseRule } from "../api";

// The daemon's built-in defaults, mirrored here so the editor can show
// them as the starting point and offer a "reset to defaults" action.
// Keep in sync with scoring.DefaultRules() on the server.
const DEFAULT_RULES: ReleaseRule[] = [
  { label: "1080p", on: "title", pattern: "\\b1080p?\\b", points: 100 },
  { label: "4K / 2160p", on: "title", pattern: "\\b(2160p?|3840p?|4k|uhd)\\b", points: 70 },
  { label: "720p", on: "title", pattern: "\\b720p?\\b", points: 30 },
  { label: "480p / SD", on: "title", pattern: "\\b(480p?|360p?|\\bsd\\b)\\b", points: -50 },
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
    emit([...rules, { label: "New rule", on: "title", pattern: "", points: 0 }]);
  const resetDefaults = () => emit(DEFAULT_RULES);

  return (
    <div className="rules-editor">
      <p className="settings-tip">
        Releases are ranked by SCORE: each matching rule adds its points
        (negative to penalise). Highest-scoring verified release wins; ties
        break by seeders/grabs. A <b>reject</b> rule hard-excludes any release
        it matches. Match against the release <b>title</b> (where resolution
        lives), the <b>indexer</b>/source name, or the <b>protocol</b>
        (<code>torrent</code> / <code>usenet</code> — e.g. prefer nzb).
        Scoring is additive, not tiered — order is cosmetic.
      </p>
      <div className="rules-list">
        <div className="rule-row rule-head">
          <span>Label</span>
          <span>On</span>
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
            <select
              className="rule-on"
              value={r.on ?? "title"}
              onChange={(e) =>
                update(i, {
                  on: e.target.value as "title" | "indexer" | "protocol",
                })
              }
            >
              <option value="title">title</option>
              <option value="indexer">indexer</option>
              <option value="protocol">protocol</option>
            </select>
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
