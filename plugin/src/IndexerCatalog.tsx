import { CheckIcon, ExternalIcon } from "./icons";
import { useEffect, useMemo, useState } from "react";
import {
  addCatalogIndexer,
  CatalogEntry,
  fetchIndexerCatalog,
  prowlarrProxyHref,
} from "./api";

// IndexerCatalog: add indexers without leaving forage. The list is
// Prowlarr's own definition catalog filtered to adult-CAPABLE entries
// (declared XXX categories), alphabetical, with privacy badges. No
// recommendations: which of them to use is the user's business.
//
// Public entries add with one click (their defaults are the config).
// Private/semi-private ones expand to the credential fields their
// definition declares. Anything with configuration forage doesn't render
// links into Prowlarr instead. Every add is TESTED against the real site
// before it saves, so a green result means the indexer answered.

type RowState =
  | { kind: "idle" }
  | { kind: "editing" }
  | { kind: "adding" }
  | { kind: "added" }
  | { kind: "error"; detail: string };

export default function IndexerCatalog({
  onAdded,
}: {
  // Fired after any successful add, so hosts (the wizard's IndexerCheck)
  // can refresh their counts.
  onAdded?: () => void;
}) {
  const [catalog, setCatalog] = useState<CatalogEntry[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [openRow, setOpenRow] = useState<string | null>(null);
  const [rowStates, setRowStates] = useState<Record<string, RowState>>({});
  const [fieldValues, setFieldValues] = useState<Record<string, string>>({});
  const [expanded, setExpanded] = useState(false);

  useEffect(() => {
    let cancelled = false;
    fetchIndexerCatalog()
      .then((r) => {
        if (!cancelled) setCatalog(r.catalog);
      })
      .catch((e) => {
        if (!cancelled) setLoadErr((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const filtered = useMemo(() => {
    if (!catalog) return [];
    const q = query.trim().toLowerCase();
    const list = q
      ? catalog.filter((c) => c.name.toLowerCase().includes(q))
      : catalog;
    return [...list].sort((a, b) => a.name.localeCompare(b.name));
  }, [catalog, query]);

  if (loadErr) {
    return (
      <p className="settings-tip">
        Couldn&rsquo;t load the indexer catalog: {loadErr}
      </p>
    );
  }
  if (catalog === null) {
    return <p className="settings-tip">Loading indexer catalog&hellip;</p>;
  }

  const rowState = (id: string): RowState => rowStates[id] ?? { kind: "idle" };
  const setRow = (id: string, st: RowState) =>
    setRowStates((m) => ({ ...m, [id]: st }));

  async function add(entry: CatalogEntry) {
    setRow(entry.id, { kind: "adding" });
    const values: Record<string, string> = {};
    for (const f of entry.fields) {
      const v = fieldValues[entry.id + "/" + f.name];
      if (v) values[f.name] = v;
    }
    try {
      await addCatalogIndexer(entry.id, values);
      setRow(entry.id, { kind: "added" });
      setOpenRow(null);
      onAdded?.();
    } catch (e) {
      setRow(entry.id, { kind: "error", detail: (e as Error).message });
    }
  }

  const shown = expanded ? filtered : filtered.slice(0, 8);

  return (
    <div className="idx-catalog">
      <input
        type="search"
        className="idx-catalog-search"
        placeholder={`Search ${catalog.length} adult-capable indexers…`}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
      />
      <ul className="idx-catalog-list">
        {shown.map((entry) => {
          const st = rowState(entry.id);
          const done = entry.added || st.kind === "added";
          return (
            <li key={entry.id} className="idx-catalog-row">
              <div className="idx-catalog-head">
                <span className="idx-catalog-name">
                  {entry.name}
                  <span className={"idx-badge " + entry.privacy}>
                    {entry.privacy === "semiPrivate"
                      ? "semi-private"
                      : entry.privacy}
                  </span>
                  {entry.protocol === "usenet" && (
                    <span className="idx-badge usenet">usenet</span>
                  )}
                </span>
                {done ? (
                  <span className="idx-catalog-added">
            <CheckIcon size={10} /> added
          </span>
                ) : entry.complex ? (
                  <a
                    className="idx-catalog-ext"
                    href={prowlarrProxyHref()}
                    target="_blank"
                    rel="noreferrer"
                  >
                    set up in Prowlarr <ExternalIcon size={10} />
                  </a>
                ) : entry.fields.length === 0 ? (
                  <button
                    className="setup-secondary small"
                    disabled={st.kind === "adding"}
                    onClick={() => add(entry)}
                  >
                    {st.kind === "adding" ? "Testing…" : "Add"}
                  </button>
                ) : (
                  <button
                    className="setup-secondary small"
                    onClick={() =>
                      setOpenRow(openRow === entry.id ? null : entry.id)
                    }
                  >
                    {openRow === entry.id ? "Cancel" : "Add…"}
                  </button>
                )}
              </div>
              {st.kind === "error" && (
                <div className="idx-catalog-err">{st.detail}</div>
              )}
              {openRow === entry.id && !done && (
                <div className="idx-catalog-fields">
                  {entry.fields.map((f) => (
                    <label key={f.name} className="setup-field">
                      <span>{f.label || f.name}</span>
                      <input
                        type={f.password ? "password" : "text"}
                        autoComplete="off"
                        spellCheck={false}
                        value={fieldValues[entry.id + "/" + f.name] ?? ""}
                        onChange={(e) =>
                          setFieldValues((m) => ({
                            ...m,
                            [entry.id + "/" + f.name]: e.target.value,
                          }))
                        }
                      />
                    </label>
                  ))}
                  <button
                    className="setup-secondary small"
                    disabled={st.kind === "adding"}
                    onClick={() => add(entry)}
                  >
                    {st.kind === "adding" ? "Testing…" : "Test + add"}
                  </button>
                </div>
              )}
            </li>
          );
        })}
      </ul>
      {filtered.length > shown.length && (
        <button
          className="setup-link inline"
          onClick={() => setExpanded(true)}
        >
          show all {filtered.length}
        </button>
      )}
    </div>
  );
}
