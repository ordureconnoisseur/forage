import { useEffect, useState } from "react";
import {
  adminToken,
  clearSession,
  ConfigField,
  ConfigFieldsResponse,
  ConfigPatch,
  fetchConfig,
  fetchIndexers,
  foragerBase,
  Health,
  IndexerInfo,
  login,
  mixedContentBlocked,
  ProbeResult,
  saveConfig,
  setAdminToken,
  setForagerBase,
  testSection,
} from "../api";
import { compileRules, parsePrefs } from "../releasePrefs";
import ReleaseRulesEditor from "./ReleaseRulesEditor";
import ReleasePrefsEditor from "./ReleasePrefsEditor";

// initialReleaseMode decides whether the Release-preferences section opens
// in friendly or advanced mode. An explicit saved releaseAdvanced flag
// (source "json") wins. Otherwise the existing-data guard: a daemon with
// hand-set releaseRules but no friendly releasePrefs (e.g. mini's current
// state) opens in ADVANCED so we never silently overwrite those rules with
// friendly defaults — the user opts into friendly when ready.
function initialReleaseMode(d: ConfigFieldsResponse): boolean {
  const adv = d.fields["releaseAdvanced"];
  if (adv && adv.source === "json") return adv.value === true;
  const prefs = d.fields["releasePrefs"]?.value;
  const rules = d.fields["releaseRules"]?.value;
  const prefsStr = typeof prefs === "string" ? prefs.trim() : "";
  const rulesStr = typeof rules === "string" ? rules.trim() : "";
  return !!rulesStr && !prefsStr;
}

// Forage Settings — connection details (browser-side, localStorage)
// plus daemon configuration (POSTed to forage /config and persisted
// to ./data/config.json). Daemon fields are sectioned to mirror the
// internal/config/config.go layout: Stash+StashDB, Indexer, Download
// clients, Library/placement, Advanced.

type SectionKey =
  | "connection"
  | "stash"
  | "indexer"
  | "downloads"
  | "library"
  | "releases"
  | "filtering"
  | "security"
  | "advanced";

const sensitiveFields = new Set([
  "stashApiKey",
  "stashdbApiKey",
  "prowlarrApiKey",
  "qbitPassword",
  "sabApiKey",
  "adminToken",
]);

// randomToken makes a 32-byte (64 hex char) secret with the browser's
// CSPRNG — same strength class as an *arr API key, generated client-side
// so it never has to be invented or round-tripped before use.
function randomToken(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

interface Props {
  onClose: () => void;
  // Called after the user logs out — App drops to the Login gate.
  onLoggedOut: () => void;
  health: Health | null;
}

export default function Settings({ onClose, onLoggedOut, health }: Props) {
  // Connection (localStorage-backed)
  const [apiURL, setApiURL] = useState(foragerBase());
  const [token, setToken] = useState(adminToken());

  // Daemon config (loaded from /config)
  const [data, setData] = useState<ConfigFieldsResponse | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [patch, setPatch] = useState<ConfigPatch>({});
  // Friendly release-prefs UI: the user's Prowlarr indexers + which mode the
  // Release-preferences section is in (null = not yet decided from /config).
  const [indexers, setIndexers] = useState<IndexerInfo[]>([]);
  const [releaseAdvanced, setReleaseAdvanced] = useState<boolean | null>(null);
  const [probes, setProbes] = useState<Record<string, ProbeResult>>({});
  const [saving, setSaving] = useState(false);
  const [saveMsg, setSaveMsg] = useState<string | null>(null);
  const [saveFailed, setSaveFailed] = useState(false);
  // Confirm-password field (local only — never sent; the daemon receives
  // just `password` once it matches).
  const [pwConfirm, setPwConfirm] = useState("");
  const [open, setOpen] = useState<Record<SectionKey, boolean>>({
    connection: true,
    stash: true,
    indexer: false,
    downloads: false,
    library: false,
    releases: false,
    filtering: false,
    security: false,
    advanced: false,
  });

  useEffect(() => {
    let cancelled = false;
    fetchConfig()
      .then((r) => {
        if (!cancelled) setData(r);
      })
      .catch((e) => {
        if (!cancelled) setLoadErr(e.message);
      });
    return () => {
      cancelled = true;
    };
    // Refetch whenever the API URL or admin token changes (after
    // saving connection settings, the user wants to see config from
    // the new daemon).
  }, [apiURL, token]);

  // Load the user's Prowlarr indexers for the friendly release-prefs editor.
  // Best-effort: an empty list (Prowlarr unconfigured / unreachable) just
  // hides the indexer section.
  useEffect(() => {
    let cancelled = false;
    fetchIndexers()
      .then((r) => {
        if (!cancelled) setIndexers(r);
      })
      .catch(() => {
        if (!cancelled) setIndexers([]);
      });
    return () => {
      cancelled = true;
    };
  }, [apiURL, token]);

  // Decide the Release-preferences mode once /config has loaded. Only seeds
  // it while undecided, so a later refetch (e.g. after a save) never yanks
  // the user out of a mode they switched into this session.
  useEffect(() => {
    if (data && releaseAdvanced === null) {
      setReleaseAdvanced(initialReleaseMode(data));
    }
  }, [data, releaseAdvanced]);

  const blocked = mixedContentBlocked();

  // Field helpers ──────────────────────────────────────────────────

  // setField records a typed value into the patch. Pass "" to clear
  // the field (falls back to env / default at compose time on the
  // server). Pass undefined to remove the field from the patch (means
  // "no change to server-side value").
  function setField<K extends keyof ConfigPatch>(
    key: K,
    value: ConfigPatch[K] | undefined,
  ) {
    setPatch((p) => {
      const next = { ...p } as ConfigPatch;
      if (value === undefined) {
        delete next[key];
      } else {
        (next[key] as ConfigPatch[K]) = value;
      }
      return next;
    });
    setSaveMsg(null);
  }

  // displayValue resolves what the input field should show. If the
  // user has typed something into the patch, show that; otherwise
  // show the server-loaded value (with sensitive fields masked).
  function displayValue(name: keyof ConfigPatch, field?: ConfigField): string {
    if (patch[name] !== undefined) {
      const v = patch[name];
      if (Array.isArray(v)) return v.join(",");
      return String(v);
    }
    if (!field) return "";
    if (sensitiveFields.has(String(name)) && field.hasSecret) return "";
    if (Array.isArray(field.value)) return field.value.join(",");
    return String(field.value ?? "");
  }

  // boolValue resolves a checkbox's state: the user's pending patch
  // wins, else the server-loaded value, else false.
  function boolValue(name: keyof ConfigPatch, field?: ConfigField): boolean {
    if (patch[name] !== undefined) return !!patch[name];
    if (field && typeof field.value === "boolean") return field.value;
    return false;
  }

  // hasSecretPlaceholder asks whether a sensitive field should show
  // the "••••••" placeholder because the daemon has a stored value
  // that the user hasn't touched in this session.
  function hasSecretPlaceholder(
    name: keyof ConfigPatch,
    field?: ConfigField,
  ): boolean {
    if (patch[name] !== undefined) return false;
    return !!field?.hasSecret;
  }

  // Test handlers ──────────────────────────────────────────────────

  async function runTest(section: string) {
    setProbes((p) => ({ ...p, [section]: { ok: false, message: "testing…" } }));
    try {
      const result = await testSection(section, patch);
      setProbes((p) => ({ ...p, [section]: result }));
    } catch (e) {
      setProbes((p) => ({
        ...p,
        [section]: { ok: false, message: (e as Error).message },
      }));
    }
  }

  // Release-mode switch ─────────────────────────────────────────────

  // goAdvanced reveals the raw rule editor. It compiles the current friendly
  // prefs into releaseRules first, so the advanced view opens showing the
  // friendly-equivalent rules to hand-tune (not stale/empty), then flags the
  // rules as advanced so they stop auto-recompiling from prefs.
  function goAdvanced() {
    const prefs = parsePrefs(
      displayValue("releasePrefs", data?.fields["releasePrefs"]),
    );
    setField("releaseRules", JSON.stringify(compileRules(prefs)));
    setField("releaseAdvanced", true);
    setReleaseAdvanced(true);
  }

  // backToSimple recompiles releaseRules from the saved friendly prefs,
  // discarding any manual edits (confirmed first), and clears the advanced
  // flag so prefs drive the rules again.
  function backToSimple() {
    const ok = window.confirm(
      "Rebuild rules from your simple preferences? This discards any manual edits to the advanced rules.",
    );
    if (!ok) return;
    const prefs = parsePrefs(
      displayValue("releasePrefs", data?.fields["releasePrefs"]),
    );
    setField("releasePrefs", JSON.stringify(prefs));
    setField("releaseRules", JSON.stringify(compileRules(prefs)));
    setField("releaseAdvanced", false);
    setReleaseAdvanced(false);
  }

  // Save handlers ──────────────────────────────────────────────────

  function saveConnection() {
    setForagerBase(apiURL);
    setAdminToken(token);
    // Force a fresh load against the new URL by reloading the page —
    // simplest way to reset every in-flight fetch/state.
    location.reload();
  }

  // Log out: clear the forage_token cookie server-side and drop the stored
  // bearer token, then let App show the Login gate. Doesn't touch the
  // daemon's configured token — just this browser's credential.
  async function logout() {
    await clearSession();
    setAdminToken("");
    onLoggedOut();
  }

  async function saveDaemonConfig(force = false) {
    if (Object.keys(patch).length === 0) {
      setSaveMsg("nothing to save");
      return;
    }
    // A new password must match its confirmation before we send it.
    // (Clearing it — empty string — needs no confirm.)
    if (patch.password && patch.password !== pwConfirm) {
      setSaveFailed(true);
      setSaveMsg("passwords don't match");
      return;
    }
    setSaving(true);
    setSaveMsg(null);
    setSaveFailed(false);
    try {
      // Capture an API-key change before the patch is cleared. The save
      // POST authenticates with the CURRENT (old) key; once it succeeds
      // the daemon requires the new one, so this browser must adopt it —
      // otherwise its next request would 401. Clearing it ("") turns the
      // key off and removes the stored value.
      const tokenChange = patch.adminToken;
      // A new password sets the daemon's password login. Auth is
      // cookie-based, so to keep this browser authenticated after the save
      // we log in with the just-entered credentials (establishing the
      // session cookie) before the refetch — otherwise the gated
      // fetchConfig below would 401.
      const pwChange = patch.password;
      const userForLogin =
        patch.username !== undefined
          ? patch.username
          : ((data?.fields["username"]?.value as string) || "");
      const r = await saveConfig(patch, { force });
      if (r.results) setProbes(r.results);
      if (!r.ok) {
        setSaveFailed(true);
        setSaveMsg(r.error || "save failed");
      } else {
        if (tokenChange !== undefined) {
          setAdminToken(tokenChange);
          setToken(tokenChange); // keep the Connection field in sync
        }
        if (pwChange) {
          try {
            await login(userForLogin, pwChange);
          } catch {
            // Couldn't auto-establish the session — the user can re-login
            // from the gate. The refetch below may 401 and bounce there.
          }
        }
        // Server reloaded; clear the patch and refetch (now authenticated
        // with the new key/cookie, if one was set).
        setPatch({});
        setPwConfirm("");
        const note = pwChange
          ? "saved — password updated"
          : tokenChange !== undefined
            ? "saved — API key updated"
            : "saved";
        setSaveMsg(note);
        const fresh = await fetchConfig();
        setData(fresh);
      }
    } catch (e) {
      setSaveFailed(true);
      setSaveMsg((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  // Render ─────────────────────────────────────────────────────────

  return (
    <div className="settings-modal" onClick={onClose}>
      <div className="settings-panel wide" onClick={(e) => e.stopPropagation()}>
        <div className="settings-head">
          <h2>Forage Settings</h2>
          <button className="settings-close" onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>

        {blocked && (
          <div className="settings-warn">
            ⚠ You're on HTTPS and the forage URL is HTTP. Browser will block all
            requests until you set an HTTPS URL or open Stash via HTTP.
          </div>
        )}

        {/* ── Connection (localStorage) ─────────────────────────── */}
        <Section
          title="Connection"
          isOpen={open.connection}
          onToggle={() =>
            setOpen((o) => ({ ...o, connection: !o.connection }))
          }
        >
          <Field label="Forage API URL">
            <input
              type="url"
              value={apiURL}
              onChange={(e) => setApiURL(e.target.value)}
              spellCheck={false}
              placeholder="https://forage.example.com"
            />
          </Field>
          {health?.adminAuthRequired && (
            <Field label="API key">
              <SecretInput
                value={token}
                onChange={setToken}
                placeholder="Bearer API key (clients/scripts)"
              />
            </Field>
          )}
          <p className="settings-tip">
            These live in your browser (localStorage), not on the daemon.
            Saving here reloads the plugin.
          </p>
          <div className="settings-actions inline">
            <button
              className="settings-save"
              onClick={saveConnection}
              disabled={apiURL === foragerBase() && token === adminToken()}
            >
              Save connection
            </button>
          </div>
        </Section>

        {/* ── Daemon config sections ────────────────────────────── */}
        {loadErr && (
          <div className="settings-warn">
            Could not load daemon config: {loadErr}
          </div>
        )}

        <Section
          title="Stash + StashDB"
          isOpen={open.stash}
          onToggle={() => setOpen((o) => ({ ...o, stash: !o.stash }))}
          probe={probes["stash"]}
          subProbe={probes["stashdb"]}
        >
          <Field label="Stash URL">
            <input
              type="url"
              value={displayValue("stashUrl", data?.fields["stashUrl"])}
              onChange={(e) => setField("stashUrl", e.target.value)}
              placeholder="https://stash.example.com"
            />
            <SourceBadge field={data?.fields["stashUrl"]} />
          </Field>
          <Field label="Stash API key">
            <SecretInput
              value={displayValue("stashApiKey", data?.fields["stashApiKey"])}
              onChange={(v) => setField("stashApiKey", v)}
              placeholder={
                hasSecretPlaceholder("stashApiKey", data?.fields["stashApiKey"])
                  ? "••••••••  (set; leave blank to keep)"
                  : "API key"
              }
            />
            <SourceBadge field={data?.fields["stashApiKey"]} />
          </Field>
          <Field label="StashDB URL">
            <input
              type="url"
              value={displayValue("stashdbUrl", data?.fields["stashdbUrl"])}
              onChange={(e) => setField("stashdbUrl", e.target.value)}
              placeholder="https://stashdb.org"
            />
            <SourceBadge field={data?.fields["stashdbUrl"]} />
          </Field>
          <Field label="StashDB API key">
            <SecretInput
              value={displayValue(
                "stashdbApiKey",
                data?.fields["stashdbApiKey"],
              )}
              onChange={(v) => setField("stashdbApiKey", v)}
              placeholder={
                hasSecretPlaceholder(
                  "stashdbApiKey",
                  data?.fields["stashdbApiKey"],
                )
                  ? "••••••••  (set; leave blank to keep)"
                  : "API key"
              }
            />
            <SourceBadge field={data?.fields["stashdbApiKey"]} />
          </Field>
          <div className="settings-actions inline">
            <button className="settings-test" onClick={() => runTest("stash")}>
              Test Stash
            </button>
            <button
              className="settings-test"
              onClick={() => runTest("stashdb")}
            >
              Test StashDB
            </button>
          </div>
        </Section>

        <Section
          title="Indexer (Prowlarr)"
          isOpen={open.indexer}
          onToggle={() => setOpen((o) => ({ ...o, indexer: !o.indexer }))}
          probe={probes["prowlarr"]}
        >
          <Field label="Prowlarr URL">
            <input
              type="url"
              value={displayValue("prowlarrUrl", data?.fields["prowlarrUrl"])}
              onChange={(e) => setField("prowlarrUrl", e.target.value)}
              placeholder="http://host.docker.internal:9696"
            />
            <SourceBadge field={data?.fields["prowlarrUrl"]} />
          </Field>
          <Field label="Prowlarr API key">
            <SecretInput
              value={displayValue(
                "prowlarrApiKey",
                data?.fields["prowlarrApiKey"],
              )}
              onChange={(v) => setField("prowlarrApiKey", v)}
              placeholder={
                hasSecretPlaceholder(
                  "prowlarrApiKey",
                  data?.fields["prowlarrApiKey"],
                )
                  ? "••••••••  (set; leave blank to keep)"
                  : "API key"
              }
            />
            <SourceBadge field={data?.fields["prowlarrApiKey"]} />
          </Field>
          <Field label="Categories (comma-separated)">
            <input
              type="text"
              value={displayValue(
                "prowlarrCategories",
                data?.fields["prowlarrCategories"],
              )}
              onChange={(e) =>
                setField(
                  "prowlarrCategories",
                  e.target.value
                    .split(",")
                    .map((s) => parseInt(s.trim(), 10))
                    .filter((n) => !isNaN(n)),
                )
              }
              placeholder="6000,6010,6020,6030,6040"
              spellCheck={false}
            />
            <SourceBadge field={data?.fields["prowlarrCategories"]} />
          </Field>
          <div className="settings-actions inline">
            <button
              className="settings-test"
              onClick={() => runTest("prowlarr")}
            >
              Test Prowlarr
            </button>
          </div>
        </Section>

        <Section
          title="Download clients"
          isOpen={open.downloads}
          onToggle={() => setOpen((o) => ({ ...o, downloads: !o.downloads }))}
          probe={probes["qbit"]}
          subProbe={probes["sab"]}
        >
          <h4 className="settings-subhead">qBittorrent</h4>
          <Field label="qBit URL">
            <input
              type="url"
              value={displayValue("qbitUrl", data?.fields["qbitUrl"])}
              onChange={(e) => setField("qbitUrl", e.target.value)}
              placeholder="http://host.docker.internal:8083"
            />
            <SourceBadge field={data?.fields["qbitUrl"]} />
          </Field>
          <Field label="qBit username">
            <input
              type="text"
              value={displayValue("qbitUsername", data?.fields["qbitUsername"])}
              onChange={(e) => setField("qbitUsername", e.target.value)}
              placeholder="leave blank for bypass_local_auth"
            />
            <SourceBadge field={data?.fields["qbitUsername"]} />
          </Field>
          <Field label="qBit password">
            <SecretInput
              value={displayValue("qbitPassword", data?.fields["qbitPassword"])}
              onChange={(v) => setField("qbitPassword", v)}
              placeholder={
                hasSecretPlaceholder("qbitPassword", data?.fields["qbitPassword"])
                  ? "••••••••  (set; leave blank to keep)"
                  : "password"
              }
            />
            <SourceBadge field={data?.fields["qbitPassword"]} />
          </Field>
          <Field label="qBit category">
            <input
              type="text"
              value={displayValue("qbitCategory", data?.fields["qbitCategory"])}
              onChange={(e) => setField("qbitCategory", e.target.value)}
              placeholder="forager"
            />
            <SourceBadge field={data?.fields["qbitCategory"]} />
          </Field>
          <div className="settings-actions inline">
            <button className="settings-test" onClick={() => runTest("qbit")}>
              Test qBit
            </button>
          </div>

          <h4 className="settings-subhead">SABnzbd</h4>
          <Field label="SAB URL">
            <input
              type="url"
              value={displayValue("sabUrl", data?.fields["sabUrl"])}
              onChange={(e) => setField("sabUrl", e.target.value)}
              placeholder="http://host.docker.internal:8080"
            />
            <SourceBadge field={data?.fields["sabUrl"]} />
          </Field>
          <Field label="SAB API key">
            <SecretInput
              value={displayValue("sabApiKey", data?.fields["sabApiKey"])}
              onChange={(v) => setField("sabApiKey", v)}
              placeholder={
                hasSecretPlaceholder("sabApiKey", data?.fields["sabApiKey"])
                  ? "••••••••  (set; leave blank to keep)"
                  : "API key"
              }
            />
            <SourceBadge field={data?.fields["sabApiKey"]} />
          </Field>
          <Field label="SAB category">
            <input
              type="text"
              value={displayValue("sabCategory", data?.fields["sabCategory"])}
              onChange={(e) => setField("sabCategory", e.target.value)}
              placeholder="forager"
            />
            <SourceBadge field={data?.fields["sabCategory"]} />
          </Field>
          <Field label="Delete after placement">
            <label className="check">
              <input
                type="checkbox"
                checked={boolValue(
                  "sabDeleteAfterPlace",
                  data?.fields["sabDeleteAfterPlace"],
                )}
                onChange={(e) =>
                  setField("sabDeleteAfterPlace", e.target.checked)
                }
              />
              Remove the SAB download once forage has placed it
            </label>
            <SourceBadge field={data?.fields["sabDeleteAfterPlace"]} />
          </Field>
          <p className="settings-tip">
            Usenet doesn't seed, so the SAB copy is redundant once the
            file is in your library. Deletes the history entry and the
            downloaded files — safe, since placement hardlinks/copies
            into the library first. Torrents are never touched (they
            keep seeding).
          </p>
          <div className="settings-actions inline">
            <button className="settings-test" onClick={() => runTest("sab")}>
              Test SAB
            </button>
          </div>
        </Section>

        <Section
          title="Library / placement"
          isOpen={open.library}
          onToggle={() => setOpen((o) => ({ ...o, library: !o.library }))}
          probe={probes["placement"]}
        >
          <Field label="Library root">
            <input
              type="text"
              value={displayValue("libraryRoot", data?.fields["libraryRoot"])}
              onChange={(e) => setField("libraryRoot", e.target.value)}
              placeholder="/data/media/library"
              spellCheck={false}
            />
            <SourceBadge field={data?.fields["libraryRoot"]} />
          </Field>
          <p className="settings-tip">
            Path inside the forage container. Must be on the same
            filesystem as the qBit + SAB complete dirs for hardlinks to
            work — otherwise the placer falls back to copy. Leave blank
            to disable placement (files stay where the client put them).
          </p>
          <Field label="Stash path mapping">
            <input
              type="text"
              value={displayValue(
                "stashPathMapping",
                data?.fields["stashPathMapping"],
              )}
              onChange={(e) => setField("stashPathMapping", e.target.value)}
              placeholder="/data/media/Media:Z:\Media"
              spellCheck={false}
            />
            <SourceBadge field={data?.fields["stashPathMapping"]} />
          </Field>
          <p className="settings-tip">
            Optional. Translates a forage-container path to the path
            Stash sees for the same file when forage triggers a scan
            after placement. Format <code>forage-prefix:stash-prefix</code>
            — e.g. forage mounts the NAS at <code>/data/media/Media</code>
            but Stash on Windows sees it as <code>Z:\Media</code>.
            Leave blank to fall back to a full-library scan after each
            placement (slower but works regardless of mount layout).
          </p>
          <Field label="Pack duplicates">
            <select
              value={
                displayValue(
                  "packDedupKeep",
                  data?.fields["packDedupKeep"],
                ) || "existing"
              }
              onChange={(e) => setField("packDedupKeep", e.target.value)}
            >
              <option value="existing">Keep my existing copy</option>
              <option value="pack">Keep the pack's copy</option>
              <option value="both">Keep both (no dedup)</option>
            </select>
            <SourceBadge field={data?.fields["packDedupKeep"]} />
          </Field>
          <p className="settings-tip">
            When a pack contains a scene you already own, which copy
            survives. Removing a copy deletes its file; the torrent keeps
            seeding from the download client's own copy regardless. Pack
            copies are often re-encodes, so "keep my existing copy" is the
            safe default.
          </p>
          <div className="settings-actions inline">
            <button
              className="settings-test"
              onClick={() => runTest("placement")}
            >
              Test library
            </button>
          </div>
        </Section>

        <Section
          title="Release preferences"
          isOpen={open.releases}
          onToggle={() => setOpen((o) => ({ ...o, releases: !o.releases }))}
        >
          <div className="prefs-mode-bar">
            {releaseAdvanced ? (
              <button
                type="button"
                className="prefs-mode-toggle"
                onClick={backToSimple}
              >
                ← Back to simple
              </button>
            ) : (
              <button
                type="button"
                className="prefs-mode-toggle"
                onClick={goAdvanced}
                disabled={releaseAdvanced === null}
              >
                Advanced ⚙
              </button>
            )}
          </div>
          {releaseAdvanced === null ? (
            <p className="settings-tip">Loading preferences…</p>
          ) : releaseAdvanced ? (
            <ReleaseRulesEditor
              value={displayValue("releaseRules", data?.fields["releaseRules"])}
              onChange={(json) => setField("releaseRules", json)}
            />
          ) : (
            <ReleasePrefsEditor
              value={displayValue("releasePrefs", data?.fields["releasePrefs"])}
              indexers={indexers}
              onChange={(prefsJSON, rulesJSON) => {
                setField("releasePrefs", prefsJSON);
                setField("releaseRules", rulesJSON);
              }}
            />
          )}
        </Section>

        <Section
          title="Scene filtering"
          isOpen={open.filtering}
          onToggle={() => setOpen((o) => ({ ...o, filtering: !o.filtering }))}
        >
          <Field label="Exclude StashDB tags">
            <ExcludedTagsEditor
              value={
                (patch.excludedSceneTags !== undefined
                  ? patch.excludedSceneTags
                  : (data?.fields["excludedSceneTags"]?.value as
                      | string[]
                      | undefined)) ?? []
              }
              onChange={(tags) => setField("excludedSceneTags", tags)}
            />
            <SourceBadge field={data?.fields["excludedSceneTags"]} />
          </Field>
          <p className="settings-tip">
            Scenes carrying any of these StashDB tags are dropped from the
            missing-scenes view and from the owned/missing counts — so
            compilations, PMVs and the like don't clutter the gap analysis or
            skew your completion stats. Matched case-insensitively; names must
            match StashDB's tag names.
          </p>
        </Section>

        <Section
          title="Security"
          isOpen={open.security}
          onToggle={() => setOpen((o) => ({ ...o, security: !o.security }))}
        >
          <h4 className="settings-subhead">Login (username + password)</h4>
          <Field label="Username">
            <input
              type="text"
              value={displayValue("username", data?.fields["username"])}
              onChange={(e) => setField("username", e.target.value)}
              placeholder="username"
              spellCheck={false}
              autoComplete="username"
            />
            <SourceBadge field={data?.fields["username"]} />
          </Field>
          <Field label="Password">
            <SecretInput
              value={patch.password ?? ""}
              onChange={(v) => setField("password", v)}
              placeholder={
                data?.fields["passwordHash"]?.hasSecret
                  ? "•••••••• (set; leave blank to keep)"
                  : "no password — set one to enable login"
              }
            />
          </Field>
          <Field label="Confirm password">
            <SecretInput
              value={pwConfirm}
              onChange={setPwConfirm}
              placeholder="re-enter password"
            />
          </Field>
          <p className="settings-tip">
            The username + password you sign in with — the normal web login.
            The daemon stores only a bcrypt hash, never the password itself.
            Leave the password blank to keep the current one; clear it (type
            a space then delete, then save with the field empty) only via{" "}
            <strong>Save changes</strong>. After setting a password this
            browser stays signed in automatically; other devices sign in
            with the same credentials.
          </p>

          <h4 className="settings-subhead">API key</h4>
          <Field label="API key">
            <div className="token-row">
              <SecretInput
                value={displayValue("adminToken", data?.fields["adminToken"])}
                onChange={(v) => setField("adminToken", v)}
                placeholder={
                  hasSecretPlaceholder("adminToken", data?.fields["adminToken"])
                    ? "•••••••• (set; leave blank to keep)"
                    : "no key — programmatic access open"
                }
              />
              <button
                type="button"
                className="settings-test"
                onClick={() => setField("adminToken", randomToken())}
                title="Generate a strong random key"
              >
                Generate
              </button>
            </div>
            <SourceBadge field={data?.fields["adminToken"]} />
          </Field>
          <p className="settings-tip">
            A shared secret for programmatic clients and scripts, sent as{" "}
            <code>Authorization: Bearer &lt;key&gt;</code> — like an *arr API
            key. Separate from the web login above. While both it and the
            password are blank, anyone who can reach the daemon can browse
            your library and submit grabs, so set a login if forage is
            reachable beyond a trusted network. <strong>Generate</strong> →{" "}
            <strong>Save changes</strong> and this browser adopts the new key
            automatically. Recover a lost key from{" "}
            <code>data/config.json</code> on the daemon host. An env{" "}
            <code>FORAGER_ADMIN_TOKEN</code> still applies when this is blank.
          </p>
          {health?.adminAuthRequired && (
            <>
              <div className="settings-actions inline">
                <button className="settings-logout" onClick={logout}>
                  Log out
                </button>
              </div>
              <p className="settings-tip">
                Forgets this browser's token and returns to the login screen.
                Doesn't change the daemon's token — other devices stay signed
                in.
              </p>
            </>
          )}
        </Section>

        <Section
          title="Advanced"
          isOpen={open.advanced}
          onToggle={() => setOpen((o) => ({ ...o, advanced: !o.advanced }))}
        >
          <Field label="Poll interval">
            <input
              type="text"
              value={displayValue("pollInterval", data?.fields["pollInterval"])}
              onChange={(e) => setField("pollInterval", e.target.value)}
              placeholder="60s"
            />
            <SourceBadge field={data?.fields["pollInterval"]} />
          </Field>
          <Field label="Orphan after">
            <input
              type="text"
              value={displayValue("orphanAfter", data?.fields["orphanAfter"])}
              onChange={(e) => setField("orphanAfter", e.target.value)}
              placeholder="6h"
            />
            <SourceBadge field={data?.fields["orphanAfter"]} />
          </Field>
          <Field label="Cache refresh">
            <input
              type="text"
              value={displayValue("cacheRefresh", data?.fields["cacheRefresh"])}
              onChange={(e) => setField("cacheRefresh", e.target.value)}
              placeholder="6h"
            />
            <SourceBadge field={data?.fields["cacheRefresh"]} />
          </Field>
          <Field label="CORS allowed origin">
            <input
              type="text"
              value={displayValue("allowedOrigin", data?.fields["allowedOrigin"])}
              onChange={(e) => setField("allowedOrigin", e.target.value)}
              placeholder="*"
            />
            <SourceBadge field={data?.fields["allowedOrigin"]} />
          </Field>
        </Section>

        <div className="settings-footer">
          {saveMsg && (
            <span
              className={"settings-msg" + (saveFailed ? " err" : " ok")}
            >
              {saveMsg}
            </span>
          )}
          <button className="settings-cancel" onClick={onClose}>
            Cancel
          </button>
          <button
            className="settings-save"
            onClick={() => saveDaemonConfig(false)}
            disabled={saving || Object.keys(patch).length === 0}
          >
            {saving ? "Saving…" : "Save changes"}
          </button>
          {saveFailed && (
            <button
              className="settings-save force"
              onClick={() => saveDaemonConfig(true)}
              disabled={saving}
              title="Save anyway, ignoring failed probes"
            >
              Force save
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

// ── Helpers ────────────────────────────────────────────────────────

function Section({
  title,
  isOpen,
  onToggle,
  probe,
  subProbe,
  children,
}: {
  title: string;
  isOpen: boolean;
  onToggle: () => void;
  probe?: ProbeResult;
  subProbe?: ProbeResult;
  children: React.ReactNode;
}) {
  return (
    <div className={"settings-section" + (isOpen ? " open" : "")}>
      <button className="settings-section-head" onClick={onToggle}>
        <span className="caret">{isOpen ? "▼" : "▶"}</span>
        <span className="title">{title}</span>
        {probe && <ProbeChip result={probe} />}
        {subProbe && <ProbeChip result={subProbe} />}
      </button>
      {isOpen && <div className="settings-section-body">{children}</div>}
    </div>
  );
}

function Field({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <label className="settings-row">
      <span>{label}</span>
      <div className="settings-row-input">{children}</div>
    </label>
  );
}

// Common StashDB noise tags, offered as one-click suggestions so the user
// doesn't have to know exact names. Matching on the backend is
// case-insensitive, so close spellings still work.
const TAG_SUGGESTIONS = [
  "Compilation",
  "Porn Music Video",
  "PMV",
  "Music Video",
];

function ExcludedTagsEditor({
  value,
  onChange,
}: {
  value: string[];
  onChange: (tags: string[]) => void;
}) {
  const [draft, setDraft] = useState("");
  const has = (t: string) =>
    value.some((v) => v.toLowerCase() === t.toLowerCase());
  const add = (t: string) => {
    const trimmed = t.trim();
    if (trimmed && !has(trimmed)) onChange([...value, trimmed]);
    setDraft("");
  };
  const remove = (t: string) => onChange(value.filter((v) => v !== t));
  const suggestions = TAG_SUGGESTIONS.filter((t) => !has(t));

  return (
    <div className="tags-editor">
      <div className="tags-chips">
        {value.map((t) => (
          <span key={t} className="tag-chip">
            {t}
            <button
              type="button"
              onClick={() => remove(t)}
              aria-label={`Remove ${t}`}
            >
              ×
            </button>
          </span>
        ))}
        <input
          className="tags-input"
          value={draft}
          placeholder={value.length ? "add a tag…" : "e.g. Compilation"}
          spellCheck={false}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              add(draft);
            } else if (e.key === "Backspace" && !draft && value.length) {
              remove(value[value.length - 1]);
            }
          }}
        />
      </div>
      {suggestions.length > 0 && (
        <div className="tags-suggest">
          <span className="tags-suggest-label">Suggested:</span>
          {suggestions.map((t) => (
            <button
              key={t}
              type="button"
              className="tag-suggest-chip"
              onClick={() => add(t)}
            >
              + {t}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function SecretInput({
  value,
  onChange,
  placeholder,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  const [shown, setShown] = useState(false);
  return (
    <div className="secret-input">
      <input
        type={shown ? "text" : "password"}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        spellCheck={false}
        autoComplete="off"
      />
      <button
        type="button"
        className="secret-toggle"
        onClick={() => setShown((s) => !s)}
      >
        {shown ? "hide" : "show"}
      </button>
    </div>
  );
}

function SourceBadge({ field }: { field?: ConfigField }) {
  if (!field) return null;
  // Only call attention to env overrides — those mean a hand-edited
  // .env value is overriding the user's UI saves (or being used in
  // place of a UI save). Default + json are the boring cases.
  if (field.source !== "env") return null;
  return (
    <span className="source-badge env" title="Value coming from .env, not config.json">
      env
    </span>
  );
}

function ProbeChip({ result }: { result: ProbeResult }) {
  return (
    <span
      className={"probe-chip " + (result.ok ? "ok" : "err")}
      title={result.message || ""}
    >
      {result.ok ? "✓" : "✗"}
    </span>
  );
}
