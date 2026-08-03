import { useEffect, useMemo, useState } from "react";
import {
  fetchPerformers,
  fetchUnfiled,
  fileUnfiled,
  suggestUnfiledPerformers,
  type UnfiledResponse,
  type UnfiledScene,
  type UnfiledSuggestion,
} from "../api";

// Unfiled: library scenes that are not under a performer folder.
//
// Three buckets, because the work each implies is different, and one of them
// does not drain. `unknown` is content no metadata source has ever identified,
// which for a library that is largely amateur and OnlyFans material is
// PERMANENT rather than pending. So it is browsable and counted, and it is
// deliberately not a badge, not a "needs attention" number, and not sorted to
// the top: a figure that can never reach zero, presented as work, gets ignored
// within a fortnight and takes the two actionable counts beside it down with
// it.
//
// The list is Stash-driven. Every count forage produced before this asked its
// grabs table, which knows only what forage fetched: 560 rows against 5,325
// scenes under Unsorted, and none of the 194 files that were lying loose in
// the library root.

type Bucket = "filable" | "identified" | "unknown";

// How many rows to mount at once.
//
// The unidentified bucket holds 4,224 scenes on the reference library and will
// only grow. Every row is a <li> with a checkbox and two lines of text, so
// mounting them all is tens of thousands of DOM nodes on a screen whose whole
// purpose is browsing. The same review that landed the verifier panel rejected
// its first version for exactly this.
//
// A cap rather than virtualisation because the interaction here is "filter
// down to what you are looking for, select a handful, file them", not "scroll
// 4,000 rows". The filter is the tool; this is the guard rail.
const RENDER_CAP = 300;

const BUCKETS: { key: Bucket; label: string; blurb: string }[] = [
  {
    key: "filable",
    label: "Ready to file",
    blurb:
      "Stash names a performer, so these can be filed now. Normally empty: the daemon files these itself when a grab confirms.",
  },
  {
    key: "identified",
    label: "No performer",
    blurb:
      "A metadata source knows the scene but nobody is attached to it. Add a performer in Stash, or file it by hand below.",
  },
  {
    key: "unknown",
    label: "Unidentified",
    blurb:
      "Nothing has ever identified these. For amateur and subscription content that is usually permanent, so this is a place to browse rather than a queue to clear.",
  },
];

// leaf trims the library root off a path so the table reads as filenames
// rather than as 90 characters of identical prefix.
function leaf(path: string, root: string): string {
  let p = path;
  if (root && p.toLowerCase().startsWith(root.toLowerCase())) {
    p = p.slice(root.length);
  }
  return p.replace(/^[\\/]+/, "");
}

export default function UnfiledList() {
  const [bucket, setBucket] = useState<Bucket>("identified");
  const [data, setData] = useState<UnfiledResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [q, setQ] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [performer, setPerformer] = useState("");
  const [busy, setBusy] = useState(false);
  const [toast, setToast] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  // Ranked guesses for the CURRENT selection, from the same suggest.Performers
  // the grab detail uses. A plain text box asked the user to remember and
  // retype a name the daemon can already read off the filename.
  const [suggestions, setSuggestions] = useState<UnfiledSuggestion[]>([]);
  // Every local performer, for autocomplete. The ranked chips cover the likely
  // answer; this covers the rest, so nobody has to spell a name from memory.
  const [allPerformers, setAllPerformers] = useState<string[]>([]);

  useEffect(() => {
    fetchPerformers({ sort: "scene_count" })
      .then((r) => setAllPerformers(r.performers.map((p) => p.name).filter(Boolean)))
      .catch(() => undefined);
  }, []);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    fetchUnfiled(bucket)
      .then((r) => {
        if (cancelled) return;
        setData(r);
        setError(null);
        setSelected(new Set());
      })
      .catch((e) => !cancelled && setError((e as Error).message))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [bucket, reloadKey]);

  const matching = useMemo(() => {
    const all = data?.scenes ?? [];
    const needle = q.trim().toLowerCase();
    if (!needle) return all;
    return all.filter(
      (s) =>
        s.path.toLowerCase().includes(needle) ||
        s.name.toLowerCase().includes(needle) ||
        (s.performers ?? []).some((p) => p.toLowerCase().includes(needle)),
    );
  }, [data, q]);
  const rows = useMemo(() => matching.slice(0, RENDER_CAP), [matching]);
  const hidden = matching.length - rows.length;

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  // The destination defaults to the suggestion when every selected row agrees,
  // which is the common case for a performer whose scenes all landed loose.
  // It stays editable, because the suggestion is Stash's opinion and the point
  // of this screen is that Stash does not always have one.
  const commonSuggestion = useMemo(() => {
    const names = new Set(
      matching
        .filter((r) => selected.has(r.key) && r.suggested)
        .map((r) => r.suggested!),
    );
    return names.size === 1 ? [...names][0] : "";
  }, [matching, selected]);

  useEffect(() => {
    if (commonSuggestion) setPerformer(commonSuggestion);
  }, [commonSuggestion]);

  // Ask the daemon who these files look like, whenever the selection changes.
  // Debounced: ticking through a dozen rows should not be a dozen round trips,
  // and each one re-reads the unfiled set server-side.
  useEffect(() => {
    const ids = [...selected];
    if (ids.length === 0) {
      setSuggestions([]);
      return;
    }
    let cancelled = false;
    const t = window.setTimeout(() => {
      suggestUnfiledPerformers(ids)
        .then((r) => !cancelled && setSuggestions(r.suggestions ?? []))
        .catch(() => !cancelled && setSuggestions([]));
    }, 250);
    return () => {
      cancelled = true;
      window.clearTimeout(t);
    };
  }, [selected]);

  const file = async () => {
    const ids = [...selected];
    const name = performer.trim();
    if (!ids.length || !name || busy) return;
    setBusy(true);
    try {
      const r = await fileUnfiled(ids, name);
      const failed = r.results.filter((x) => x.error);
      setToast(
        failed.length === 0
          ? `Filed ${r.moved} under ${name}. Stash is rescanning that folder.`
          : `Filed ${r.moved}, skipped ${r.skipped}. First reason: ${failed[0].error}`,
      );
      setReloadKey((k) => k + 1);
    } catch (e) {
      setToast("Couldn't file: " + (e as Error).message);
    } finally {
      setBusy(false);
      window.setTimeout(() => setToast(null), 6000);
    }
  };

  // What the button is actually about to move. One row can be 489 files.
  const movingFiles = matching
    .filter((r) => selected.has(r.key))
    .reduce((n, r) => n + (r.files || 1), 0);

  const counts = data?.counts ?? {};
  const root = data?.library_root ?? "";
  const active = BUCKETS.find((b) => b.key === bucket)!;

  return (
    <div>
      <div className="page-header">
        <h2>Unfiled</h2>
        <div className="meta">
          Library scenes that are not under a performer folder.
        </div>
      </div>

      <div className="controls unfiled-buckets" role="group" aria-label="Bucket">
        {BUCKETS.map((b) => (
          <button
            key={b.key}
            type="button"
            className={"grab-chip" + (b.key === bucket ? " active chip-any" : "")}
            onClick={() => setBucket(b.key)}
          >
            <span className="chip-label">{b.label}</span>
            <span className="unfiled-count">{counts[b.key] ?? 0}</span>
          </button>
        ))}
      </div>
      <p className="unfiled-blurb">{active.blurb}</p>

      <div className="controls">
        <input
          type="text"
          placeholder="Filter by filename, title, performer…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <span className="count">
          {matching.length} / {data?.scenes.length ?? 0}
        </span>
      </div>

      {toast && <div className="ms-toast">{toast}</div>}
      {error && <div className="empty error">Couldn't load: {error}</div>}
      {loading && !data && <div className="empty">Loading…</div>}

      {data && rows.length === 0 && !loading && (
        <div className="empty">
          {bucket === "filable"
            ? "Nothing waiting to be filed, which is what this bucket should normally look like."
            : "Nothing here."}
        </div>
      )}

      {rows.length > 0 && (
        <>
          <ul className="watch-list unfiled-list">
            {rows.map((s) => (
              <UnfiledRow
                key={s.key}
                s={s}
                root={root}
                checked={selected.has(s.key)}
                onToggle={() => toggle(s.key)}
              />
            ))}
          </ul>

          {hidden > 0 && (
            <p className="unfiled-blurb unfiled-capped">
              Showing the first {RENDER_CAP} of {matching.length}. Narrow the
              filter to reach the rest: this is a browsing surface, and
              mounting several thousand rows to scroll past them helps nobody.
            </p>
          )}

          {/* The action bar only exists once something is selected: an
              always-present destination field on a browsing screen invites
              filing a row you happened to be reading. */}
          {selected.size > 0 && (
            <div className="watch-foot unfiled-actions">
              {suggestions.length > 0 && (
                <div className="unfiled-suggestions">
                  {suggestions.map((p) => (
                    <button
                      key={p.stash_id || p.name}
                      type="button"
                      className={
                        "grab-chip" + (performer === p.name ? " active chip-any" : "")
                      }
                      onClick={() => setPerformer(p.name)}
                      title={`${p.scene_count} scenes in your library`}
                    >
                      <span className="chip-label">
                        {p.favorite ? "\u2665 " : ""}
                        {p.name}
                      </span>
                    </button>
                  ))}
                </div>
              )}
              <span className="unfiled-selected">{selected.size} selected</span>
              <input
                type="text"
                className="unfiled-performer"
                placeholder="File under performer…"
                list="unfiled-performer-options"
                value={performer}
                onChange={(e) => setPerformer(e.target.value)}
              />
              <datalist id="unfiled-performer-options">
                {allPerformers.map((n) => (
                  <option key={n} value={n} />
                ))}
              </datalist>
              <button
                className="collection-cta"
                disabled={busy || !performer.trim()}
                onClick={file}
                title="Move these files under that performer's folder"
              >
                {busy ? "Filing…" : `File ${selected.size} (${movingFiles} files) →`}
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function UnfiledRow({
  s,
  root,
  checked,
  onToggle,
}: {
  s: UnfiledScene;
  root: string;
  checked: boolean;
  onToggle: () => void;
}) {
  return (
    <li className={"watch-card unfiled-card" + (checked ? " is-selected" : "")}>
      <label className="unfiled-pick">
        <input type="checkbox" checked={checked} onChange={onToggle} />
      </label>
      <div className="unfiled-body">
        <div className="unfiled-name" title={s.path}>
          {s.kind === "pack" && <span className="unfiled-packtag">pack</span>}
          {leaf(s.path, root)}
        </div>
        <div className="unfiled-meta">
          {/* A pack says how much rides on the decision. Filing "Adorable
              Alice Video Pack" moves 489 files, and that should not be a
              surprise discovered afterwards. */}
          {s.kind === "pack" && (
            <span className="unfiled-files">
              {s.files} file{s.files === 1 ? "" : "s"}, moved together
            </span>
          )}
          {s.suggested && (
            <span className="unfiled-suggest">suggests {s.suggested}</span>
          )}
          {!s.identified && (
            <span className="unfiled-unknown">not identified</span>
          )}
        </div>
      </div>
    </li>
  );
}
