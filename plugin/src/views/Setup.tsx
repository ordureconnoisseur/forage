import { useState } from "react";
import {
  adminToken,
  ConfigPatch,
  fetchConfig,
  fetchHealth,
  foragerBase,
  Health,
  saveConfig,
  setAdminToken,
  setForagerBase,
  testSection,
} from "../api";
import AcornIcon from "../AcornIcon";

// First-run setup wizard — the guided alternative to dropping a new user
// into the full Settings form. Modelled on Hearth's onboarding: a step
// state machine with test-before-advance and inline result feedback.
//
// The happy path walks every section forage needs to actually grab:
//   Welcome → [Connect] → Stash+StashDB → Indexer → Download client →
//   Library → Done
// Finishing the wizard leaves a daemon that can browse → search → grab
// without ever opening Settings.
//
// forage has two setup layers Hearth doesn't: (1) Connect — point the
// plugin at the daemon (URL + token if required), browser-side, probed
// via the public /healthz; this step only shows in dev/cross-origin —
// standalone serves the app at the same origin so there's nothing to
// connect to. (2) Credentials onward — POSTed incrementally to /config
// so a half-finished setup survives a reload.
//
// Each config step pre-checks the matching /healthz flag: when a section
// is already set (e.g. via .env), it shows a green "✓ already configured"
// and lets the user breeze past with no entry. Otherwise it shows the
// fields + a Test button that must pass before Continue, plus a subtle
// "Skip for now" link.

type Step =
  | "welcome"
  | "connect"
  | "credentials"
  | "indexer"
  | "clients"
  | "library"
  | "done";
type Test =
  | { kind: "idle" }
  | { kind: "testing" }
  | { kind: "ok"; detail: string }
  | { kind: "err"; detail: string };

// normalizeUrl trims, prepends https:// when no scheme is given (forage
// daemons typically sit behind Tailscale/HTTPS), and strips trailing
// slashes. Returns "" when there's nothing usable.
function normalizeUrl(raw: string): string {
  let s = raw.trim();
  if (s === "") return "";
  if (!/^https?:\/\//i.test(s)) s = "https://" + s;
  return s.replace(/\/+$/, "");
}

// parseCats turns the comma-separated category input into the number[]
// the daemon expects, dropping anything non-numeric.
function parseCats(s: string): number[] {
  return s
    .split(",")
    .map((x) => parseInt(x.trim(), 10))
    .filter((n) => !isNaN(n));
}

// isFullyConfigured is true when the daemon already has everything the
// wizard would set — so a reconnect to a fully-provisioned daemon can
// jump straight to the finish line instead of clicking through every
// "already configured" screen.
function isFullyConfigured(h: Health | null): boolean {
  return (
    !!h &&
    !h.unconfigured &&
    h.prowlarrConfigured &&
    (h.qbitConfigured || h.sabConfigured) &&
    h.placerConfigured
  );
}

export default function Setup({
  health,
  healthError,
  sameOrigin,
  onDone,
  onAdvanced,
}: {
  health: Health | null;
  healthError: string | null;
  // True when the daemon serves this app at the same origin — there's
  // nothing to "connect" to, so the wizard skips that step entirely.
  sameOrigin: boolean;
  // Bump App's health probe so it re-evaluates needsSetup and unmounts
  // the wizard once everything's configured.
  onDone: () => void;
  // Escape hatch to the full Settings panel for anything the wizard
  // doesn't cover (release rules, scene filtering, advanced).
  onAdvanced: () => void;
}) {
  // Jump straight to the relevant step when there's already partial
  // setup: same-origin daemon → welcome → credentials (no connect step);
  // unreachable URL → reconnect; reachable but unconfigured → credentials;
  // otherwise start from the welcome screen.
  const initialStep: Step = sameOrigin
    ? "welcome"
    : !foragerBase()
      ? "welcome"
      : healthError
        ? "connect"
        : health?.unconfigured
          ? "credentials"
          : "welcome";

  const [step, setStep] = useState<Step>(initialStep);

  // liveHealth tracks the daemon's configured-state as the wizard makes
  // changes: seeded from the prop, replaced by the connect probe, and
  // refetched after each incremental save so the per-step "already
  // configured ✓" checks reflect what's actually been persisted.
  const [liveHealth, setLiveHealth] = useState<Health | null>(health);

  // Whether the credentials step belongs in the flow (and the stepper).
  // Captured from the daemon's initial state and updated by the connect
  // probe, but NOT after the credentials save — so the dot count stays
  // stable while the user walks the remaining steps.
  const [needsCredsStep, setNeedsCredsStep] = useState<boolean>(
    health ? health.unconfigured : true,
  );

  async function refreshHealth(): Promise<void> {
    try {
      setLiveHealth(await fetchHealth());
    } catch {
      // Best-effort — the next step just won't show its "configured ✓"
      // shortcut, which is harmless (the user can still enter values).
    }
  }

  // Connect step
  const [url, setUrl] = useState(foragerBase());
  const [token, setToken] = useState(adminToken());
  const [needsToken, setNeedsToken] = useState(!!health?.adminAuthRequired);
  const [connTest, setConnTest] = useState<Test>({ kind: "idle" });

  // Credentials step
  const [stashUrl, setStashUrl] = useState("");
  const [stashKey, setStashKey] = useState("");
  const [stashdbUrl, setStashdbUrl] = useState("https://stashdb.org");
  const [stashdbKey, setStashdbKey] = useState("");
  const [stashTest, setStashTest] = useState<Test>({ kind: "idle" });
  const [stashdbTest, setStashdbTest] = useState<Test>({ kind: "idle" });
  const [credErr, setCredErr] = useState<string | null>(null);
  const [savingCreds, setSavingCreds] = useState(false);

  // Indexer step
  const [prowlarrUrl, setProwlarrUrl] = useState("");
  const [prowlarrKey, setProwlarrKey] = useState("");
  const [prowlarrCats, setProwlarrCats] = useState("6000,6010,6020,6030,6040");
  const [prowlarrTest, setProwlarrTest] = useState<Test>({ kind: "idle" });
  const [indexerErr, setIndexerErr] = useState<string | null>(null);
  const [savingIndexer, setSavingIndexer] = useState(false);

  // Download-client step (qBittorrent and/or SABnzbd — either is enough)
  const [qbitUrl, setQbitUrl] = useState("");
  const [qbitUser, setQbitUser] = useState("");
  const [qbitPass, setQbitPass] = useState("");
  const [qbitCat, setQbitCat] = useState("forage");
  const [qbitTest, setQbitTest] = useState<Test>({ kind: "idle" });
  const [sabUrl, setSabUrl] = useState("");
  const [sabKey, setSabKey] = useState("");
  const [sabCat, setSabCat] = useState("forage");
  const [sabTest, setSabTest] = useState<Test>({ kind: "idle" });
  const [clientsErr, setClientsErr] = useState<string | null>(null);
  const [savingClients, setSavingClients] = useState(false);

  // Library step
  const [libraryRoot, setLibraryRoot] = useState("");
  const [stashPathMapping, setStashPathMapping] = useState("");
  const [placementTest, setPlacementTest] = useState<Test>({ kind: "idle" });
  const [libraryErr, setLibraryErr] = useState<string | null>(null);
  const [savingLibrary, setSavingLibrary] = useState(false);

  // ── Connect ──────────────────────────────────────────────────────
  async function testConnect() {
    const norm = normalizeUrl(url);
    if (!norm) {
      setConnTest({ kind: "err", detail: "That doesn't look like a URL" });
      return;
    }
    setConnTest({ kind: "testing" });
    setForagerBase(norm);
    setUrl(norm);
    let h: Health;
    try {
      // /healthz is public — tells us reachability + whether a token is
      // required + whether the daemon has credentials yet.
      h = await fetchHealth();
    } catch {
      setConnTest({
        kind: "err",
        detail: "Couldn't reach a forage daemon at that URL",
      });
      return;
    }
    setLiveHealth(h);
    setNeedsCredsStep(h.unconfigured);
    if (h.adminAuthRequired) {
      setNeedsToken(true);
      if (!token.trim()) {
        setConnTest({
          kind: "err",
          detail: "This daemon requires an admin token — paste it below",
        });
        return;
      }
      setAdminToken(token.trim());
      try {
        // /config is gated, so this proves the token is accepted.
        await fetchConfig();
      } catch {
        setConnTest({
          kind: "err",
          detail: "Token rejected — double-check it and test again",
        });
        return;
      }
    } else {
      setNeedsToken(false);
      setAdminToken(token.trim()); // harmless when empty
    }
    setConnTest({ kind: "ok", detail: `Daemon reachable (v${h.version})` });
  }

  function connectContinue() {
    // A daemon that already has everything skips straight to the finish
    // line. One with stash creds but missing pieces enters at the indexer
    // step (each step auto-detects what's already set). Otherwise start
    // from credentials.
    if (isFullyConfigured(liveHealth)) {
      finish();
    } else if (liveHealth && !liveHealth.unconfigured) {
      setStep("indexer");
    } else {
      setStep("credentials");
    }
  }

  // ── Credentials ──────────────────────────────────────────────────
  async function runStashTest() {
    setStashTest({ kind: "testing" });
    try {
      const r = await testSection("stash", {
        stashUrl,
        stashApiKey: stashKey,
      });
      setStashTest(
        r.ok
          ? { kind: "ok", detail: r.message || "Stash reachable" }
          : { kind: "err", detail: r.message || "Couldn't reach Stash" },
      );
    } catch (e) {
      setStashTest({ kind: "err", detail: (e as Error).message });
    }
  }
  async function runStashDBTest() {
    setStashdbTest({ kind: "testing" });
    try {
      const r = await testSection("stashdb", {
        stashdbUrl,
        stashdbApiKey: stashdbKey,
      });
      setStashdbTest(
        r.ok
          ? { kind: "ok", detail: r.message || "StashDB reachable" }
          : { kind: "err", detail: r.message || "Couldn't reach StashDB" },
      );
    } catch (e) {
      setStashdbTest({ kind: "err", detail: (e as Error).message });
    }
  }

  async function saveCredentials(force: boolean) {
    setSavingCreds(true);
    setCredErr(null);
    try {
      const r = await saveConfig(
        {
          stashUrl,
          stashApiKey: stashKey,
          stashdbUrl,
          stashdbApiKey: stashdbKey,
        },
        { force },
      );
      if (!r.ok) {
        setCredErr(
          r.error ||
            "Stash or StashDB couldn't be reached. Check the values, or save anyway.",
        );
        return;
      }
      await refreshHealth();
      setStep("indexer");
    } catch (e) {
      setCredErr((e as Error).message);
    } finally {
      setSavingCreds(false);
    }
  }

  // ── Indexer ──────────────────────────────────────────────────────
  async function runProwlarrTest() {
    setProwlarrTest({ kind: "testing" });
    try {
      const r = await testSection("prowlarr", {
        prowlarrUrl,
        prowlarrApiKey: prowlarrKey,
        prowlarrCategories: parseCats(prowlarrCats),
      });
      setProwlarrTest(
        r.ok
          ? { kind: "ok", detail: r.message || "Prowlarr reachable" }
          : { kind: "err", detail: r.message || "Couldn't reach Prowlarr" },
      );
    } catch (e) {
      setProwlarrTest({ kind: "err", detail: (e as Error).message });
    }
  }

  async function saveIndexer() {
    setSavingIndexer(true);
    setIndexerErr(null);
    try {
      // Only send what the user actually entered, so we don't clobber an
      // env-set value with an empty string. The Test already validated
      // these, so force past the save-time re-probe.
      const patch: ConfigPatch = {};
      if (prowlarrUrl.trim()) patch.prowlarrUrl = prowlarrUrl.trim();
      if (prowlarrKey.trim()) patch.prowlarrApiKey = prowlarrKey.trim();
      const cats = parseCats(prowlarrCats);
      if (cats.length) patch.prowlarrCategories = cats;
      const r = await saveConfig(patch, { force: true });
      if (!r.ok) {
        setIndexerErr(r.error || "Couldn't save the indexer settings.");
        return;
      }
      await refreshHealth();
      setStep("clients");
    } catch (e) {
      setIndexerErr((e as Error).message);
    } finally {
      setSavingIndexer(false);
    }
  }

  // ── Download clients ─────────────────────────────────────────────
  async function runQbitTest() {
    setQbitTest({ kind: "testing" });
    try {
      const r = await testSection("qbit", {
        qbitUrl,
        qbitUsername: qbitUser,
        qbitPassword: qbitPass,
        qbitCategory: qbitCat,
      });
      setQbitTest(
        r.ok
          ? { kind: "ok", detail: r.message || "qBittorrent reachable" }
          : { kind: "err", detail: r.message || "Couldn't reach qBittorrent" },
      );
    } catch (e) {
      setQbitTest({ kind: "err", detail: (e as Error).message });
    }
  }
  async function runSabTest() {
    setSabTest({ kind: "testing" });
    try {
      const r = await testSection("sab", {
        sabUrl,
        sabApiKey: sabKey,
        sabCategory: sabCat,
      });
      setSabTest(
        r.ok
          ? { kind: "ok", detail: r.message || "SABnzbd reachable" }
          : { kind: "err", detail: r.message || "Couldn't reach SABnzbd" },
      );
    } catch (e) {
      setSabTest({ kind: "err", detail: (e as Error).message });
    }
  }

  async function saveClients() {
    setSavingClients(true);
    setClientsErr(null);
    try {
      // Save only the client(s) whose test passed — leaving the other's
      // env/default values untouched.
      const patch: ConfigPatch = {};
      if (qbitTest.kind === "ok") {
        if (qbitUrl.trim()) patch.qbitUrl = qbitUrl.trim();
        if (qbitUser.trim()) patch.qbitUsername = qbitUser.trim();
        if (qbitPass.trim()) patch.qbitPassword = qbitPass.trim();
        if (qbitCat.trim()) patch.qbitCategory = qbitCat.trim();
      }
      if (sabTest.kind === "ok") {
        if (sabUrl.trim()) patch.sabUrl = sabUrl.trim();
        if (sabKey.trim()) patch.sabApiKey = sabKey.trim();
        if (sabCat.trim()) patch.sabCategory = sabCat.trim();
      }
      const r = await saveConfig(patch, { force: true });
      if (!r.ok) {
        setClientsErr(r.error || "Couldn't save the download client.");
        return;
      }
      await refreshHealth();
      setStep("library");
    } catch (e) {
      setClientsErr((e as Error).message);
    } finally {
      setSavingClients(false);
    }
  }

  // ── Library ──────────────────────────────────────────────────────
  async function runPlacementTest() {
    setPlacementTest({ kind: "testing" });
    try {
      const r = await testSection("placement", {
        libraryRoot,
        ...(stashPathMapping.trim() ? { stashPathMapping } : {}),
      });
      setPlacementTest(
        r.ok
          ? { kind: "ok", detail: r.message || "Library path is writable" }
          : { kind: "err", detail: r.message || "Couldn't use that path" },
      );
    } catch (e) {
      setPlacementTest({ kind: "err", detail: (e as Error).message });
    }
  }

  async function saveLibrary() {
    setSavingLibrary(true);
    setLibraryErr(null);
    try {
      const patch: ConfigPatch = {};
      if (libraryRoot.trim()) patch.libraryRoot = libraryRoot.trim();
      if (stashPathMapping.trim())
        patch.stashPathMapping = stashPathMapping.trim();
      const r = await saveConfig(patch, { force: true });
      if (!r.ok) {
        setLibraryErr(r.error || "Couldn't save the library settings.");
        return;
      }
      await refreshHealth();
      finish();
    } catch (e) {
      setLibraryErr((e as Error).message);
    } finally {
      setSavingLibrary(false);
    }
  }

  function finish() {
    setStep("done");
  }

  const credsReady =
    stashUrl.trim() !== "" && stashKey.trim() !== "" && stashdbKey.trim() !== "";

  // ── Render ───────────────────────────────────────────────────────
  return (
    <div className="setup">
      <div className="setup-card">
        {step !== "welcome" && (
          <Stepper
            step={step}
            needsCreds={needsCredsStep}
            sameOrigin={sameOrigin}
          />
        )}

        {step === "welcome" && (
          <div className="setup-welcome">
            <AcornIcon className="setup-acorn" />
            <h1>forage</h1>
            <p className="setup-tagline">
              Performer-driven scene grabbing for Stash. A few quick steps and
              you'll be ready to browse, search and grab.
            </p>
            <button
              className="setup-primary"
              onClick={() => setStep(sameOrigin ? "credentials" : "connect")}
            >
              Get started
            </button>
            <button className="setup-link" onClick={onAdvanced}>
              Use advanced settings instead
            </button>
          </div>
        )}

        {step === "connect" && (
          <div className="setup-step">
            <h2>Connect to your daemon</h2>
            <p className="setup-sub">
              The URL where the forage daemon is reachable from this browser —
              your Tailscale Serve address or reverse-proxy URL.
            </p>
            <label className="setup-field">
              <span>Daemon URL</span>
              <input
                type="url"
                value={url}
                spellCheck={false}
                placeholder="https://forage.example.ts.net"
                onChange={(e) => setUrl(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && testConnect()}
              />
            </label>
            {needsToken && (
              <label className="setup-field">
                <span>Admin token</span>
                <input
                  type="password"
                  value={token}
                  spellCheck={false}
                  autoComplete="off"
                  placeholder="this daemon requires a token"
                  onChange={(e) => setToken(e.target.value)}
                  onKeyDown={(e) => e.key === "Enter" && testConnect()}
                />
              </label>
            )}
            <TestLabel test={connTest} />
            <div className="setup-actions">
              <button
                className="setup-secondary"
                onClick={testConnect}
                disabled={!url.trim() || connTest.kind === "testing"}
              >
                {connTest.kind === "testing" ? "Testing…" : "Test connection"}
              </button>
              <button
                className="setup-primary"
                onClick={connectContinue}
                disabled={connTest.kind !== "ok"}
              >
                Continue
              </button>
            </div>
            <button className="setup-link" onClick={onAdvanced}>
              Use advanced settings instead
            </button>
          </div>
        )}

        {step === "credentials" && (
          <div className="setup-step">
            <h2>Connect Stash + StashDB</h2>
            <p className="setup-sub">
              forage needs read access to your Stash library and a StashDB API
              key to know each performer's filmography.
            </p>

            <h4 className="setup-subhead">Stash</h4>
            <label className="setup-field">
              <span>Stash URL</span>
              <input
                type="url"
                value={stashUrl}
                spellCheck={false}
                placeholder="http://host.docker.internal:9999"
                onChange={(e) => setStashUrl(e.target.value)}
              />
            </label>
            <label className="setup-field">
              <span>Stash API key</span>
              <input
                type="password"
                value={stashKey}
                spellCheck={false}
                autoComplete="off"
                placeholder="Settings → Security → API Key in Stash"
                onChange={(e) => setStashKey(e.target.value)}
              />
            </label>
            <div className="setup-inline-test">
              <button
                className="setup-secondary small"
                onClick={runStashTest}
                disabled={
                  !stashUrl.trim() || !stashKey.trim() || stashTest.kind === "testing"
                }
              >
                Test Stash
              </button>
              <TestLabel test={stashTest} />
            </div>

            <h4 className="setup-subhead">StashDB</h4>
            <label className="setup-field">
              <span>StashDB URL</span>
              <input
                type="url"
                value={stashdbUrl}
                spellCheck={false}
                onChange={(e) => setStashdbUrl(e.target.value)}
              />
            </label>
            <label className="setup-field">
              <span>StashDB API key</span>
              <input
                type="password"
                value={stashdbKey}
                spellCheck={false}
                autoComplete="off"
                placeholder="stashdb.org → Account → API key"
                onChange={(e) => setStashdbKey(e.target.value)}
              />
            </label>
            <div className="setup-inline-test">
              <button
                className="setup-secondary small"
                onClick={runStashDBTest}
                disabled={!stashdbKey.trim() || stashdbTest.kind === "testing"}
              >
                Test StashDB
              </button>
              <TestLabel test={stashdbTest} />
            </div>

            {credErr && <div className="setup-err">{credErr}</div>}
            <div className="setup-actions">
              <button
                className="setup-primary"
                onClick={() => saveCredentials(false)}
                disabled={!credsReady || savingCreds}
              >
                {savingCreds ? "Saving…" : "Continue"}
              </button>
              {credErr && (
                <button
                  className="setup-secondary"
                  onClick={() => saveCredentials(true)}
                  disabled={savingCreds}
                  title="Save anyway, ignoring the failed test"
                >
                  Save anyway
                </button>
              )}
            </div>
            <button className="setup-link" onClick={onAdvanced}>
              Use advanced settings instead
            </button>
          </div>
        )}

        {step === "indexer" && (
          <div className="setup-step">
            <h2>Add an indexer</h2>
            <p className="setup-sub">
              forage searches your Prowlarr indexers for scene releases. Point
              it at Prowlarr and choose which categories to search.
            </p>
            {liveHealth?.prowlarrConfigured ? (
              <ConfiguredNotice
                label="Prowlarr is already configured"
                onContinue={() => setStep("clients")}
              />
            ) : (
              <>
                <label className="setup-field">
                  <span>Prowlarr URL</span>
                  <input
                    type="url"
                    value={prowlarrUrl}
                    spellCheck={false}
                    placeholder="http://host.docker.internal:9696"
                    onChange={(e) => setProwlarrUrl(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>Prowlarr API key</span>
                  <input
                    type="password"
                    value={prowlarrKey}
                    spellCheck={false}
                    autoComplete="off"
                    placeholder="Prowlarr → Settings → General → API Key"
                    onChange={(e) => setProwlarrKey(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>Categories</span>
                  <input
                    type="text"
                    value={prowlarrCats}
                    spellCheck={false}
                    placeholder="6000,6010,6020,6030,6040"
                    onChange={(e) => setProwlarrCats(e.target.value)}
                  />
                </label>
                <div className="setup-inline-test">
                  <button
                    className="setup-secondary small"
                    onClick={runProwlarrTest}
                    disabled={
                      !prowlarrUrl.trim() ||
                      !prowlarrKey.trim() ||
                      prowlarrTest.kind === "testing"
                    }
                  >
                    Test Prowlarr
                  </button>
                  <TestLabel test={prowlarrTest} />
                </div>
                {indexerErr && <div className="setup-err">{indexerErr}</div>}
                <div className="setup-actions">
                  <button
                    className="setup-primary"
                    onClick={saveIndexer}
                    disabled={prowlarrTest.kind !== "ok" || savingIndexer}
                  >
                    {savingIndexer ? "Saving…" : "Continue"}
                  </button>
                </div>
                <button
                  className="setup-link"
                  onClick={() => setStep("clients")}
                >
                  Skip for now
                </button>
              </>
            )}
          </div>
        )}

        {step === "clients" && (
          <div className="setup-step">
            <h2>Add a download client</h2>
            <p className="setup-sub">
              forage sends grabs to qBittorrent (torrents) or SABnzbd (usenet).
              Set up whichever you use — one is enough to get started.
            </p>
            {liveHealth?.qbitConfigured || liveHealth?.sabConfigured ? (
              <ConfiguredNotice
                label={
                  liveHealth?.qbitConfigured && liveHealth?.sabConfigured
                    ? "qBittorrent and SABnzbd are already configured"
                    : liveHealth?.qbitConfigured
                      ? "qBittorrent is already configured"
                      : "SABnzbd is already configured"
                }
                onContinue={() => setStep("library")}
              />
            ) : (
              <>
                <h4 className="setup-subhead">qBittorrent</h4>
                <label className="setup-field">
                  <span>qBit URL</span>
                  <input
                    type="url"
                    value={qbitUrl}
                    spellCheck={false}
                    placeholder="http://host.docker.internal:8083"
                    onChange={(e) => setQbitUrl(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>qBit username</span>
                  <input
                    type="text"
                    value={qbitUser}
                    spellCheck={false}
                    placeholder="leave blank for bypass_local_auth"
                    onChange={(e) => setQbitUser(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>qBit password</span>
                  <input
                    type="password"
                    value={qbitPass}
                    spellCheck={false}
                    autoComplete="off"
                    placeholder="password"
                    onChange={(e) => setQbitPass(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>qBit category</span>
                  <input
                    type="text"
                    value={qbitCat}
                    spellCheck={false}
                    placeholder="forage"
                    onChange={(e) => setQbitCat(e.target.value)}
                  />
                </label>
                <div className="setup-inline-test">
                  <button
                    className="setup-secondary small"
                    onClick={runQbitTest}
                    disabled={!qbitUrl.trim() || qbitTest.kind === "testing"}
                  >
                    Test qBit
                  </button>
                  <TestLabel test={qbitTest} />
                </div>

                <h4 className="setup-subhead">SABnzbd</h4>
                <label className="setup-field">
                  <span>SAB URL</span>
                  <input
                    type="url"
                    value={sabUrl}
                    spellCheck={false}
                    placeholder="http://host.docker.internal:8080"
                    onChange={(e) => setSabUrl(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>SAB API key</span>
                  <input
                    type="password"
                    value={sabKey}
                    spellCheck={false}
                    autoComplete="off"
                    placeholder="SAB → Config → General → API Key"
                    onChange={(e) => setSabKey(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>SAB category</span>
                  <input
                    type="text"
                    value={sabCat}
                    spellCheck={false}
                    placeholder="forage"
                    onChange={(e) => setSabCat(e.target.value)}
                  />
                </label>
                <div className="setup-inline-test">
                  <button
                    className="setup-secondary small"
                    onClick={runSabTest}
                    disabled={
                      !sabUrl.trim() || !sabKey.trim() || sabTest.kind === "testing"
                    }
                  >
                    Test SAB
                  </button>
                  <TestLabel test={sabTest} />
                </div>

                {clientsErr && <div className="setup-err">{clientsErr}</div>}
                <div className="setup-actions">
                  <button
                    className="setup-primary"
                    onClick={saveClients}
                    disabled={
                      (qbitTest.kind !== "ok" && sabTest.kind !== "ok") ||
                      savingClients
                    }
                  >
                    {savingClients ? "Saving…" : "Continue"}
                  </button>
                </div>
                <button
                  className="setup-link"
                  onClick={() => setStep("library")}
                >
                  Skip for now
                </button>
              </>
            )}
          </div>
        )}

        {step === "library" && (
          <div className="setup-step">
            <h2>Choose your library</h2>
            <p className="setup-sub">
              Where forage places finished downloads — a path inside the forage
              container, on the same filesystem as your download client's
              complete dir so it can hardlink.
            </p>
            {liveHealth?.placerConfigured ? (
              <ConfiguredNotice
                label="Library placement is already configured"
                onContinue={finish}
              />
            ) : (
              <>
                <label className="setup-field">
                  <span>Library root</span>
                  <input
                    type="text"
                    value={libraryRoot}
                    spellCheck={false}
                    placeholder="/data/media/library"
                    onChange={(e) => setLibraryRoot(e.target.value)}
                  />
                </label>
                <label className="setup-field">
                  <span>Stash path mapping (optional)</span>
                  <input
                    type="text"
                    value={stashPathMapping}
                    spellCheck={false}
                    placeholder="/data/media/Media:Z:\Media"
                    onChange={(e) => setStashPathMapping(e.target.value)}
                  />
                </label>
                <div className="setup-inline-test">
                  <button
                    className="setup-secondary small"
                    onClick={runPlacementTest}
                    disabled={!libraryRoot.trim() || placementTest.kind === "testing"}
                  >
                    Test library
                  </button>
                  <TestLabel test={placementTest} />
                </div>
                {libraryErr && <div className="setup-err">{libraryErr}</div>}
                <div className="setup-actions">
                  <button
                    className="setup-primary"
                    onClick={saveLibrary}
                    disabled={placementTest.kind !== "ok" || savingLibrary}
                  >
                    {savingLibrary ? "Saving…" : "Finish setup"}
                  </button>
                </div>
                <button className="setup-link" onClick={finish}>
                  Skip for now
                </button>
              </>
            )}
          </div>
        )}

        {step === "done" && (
          <div className="setup-welcome">
            <div className="setup-check" aria-hidden="true">
              ✓
            </div>
            <h1>You're set</h1>
            <p className="setup-tagline">
              forage is syncing your library in the background. Browse your
              performers and start filling the gaps.
            </p>
            <button className="setup-primary" onClick={onDone}>
              Open forage
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

function Stepper({
  step,
  needsCreds,
  sameOrigin,
}: {
  step: Step;
  needsCreds: boolean;
  sameOrigin: boolean;
}) {
  // welcome isn't a dot. Same-origin drops the connect step (the daemon
  // is serving us); the credentials dot only shows when creds are still
  // needed. Indexer → clients → library → done always follow.
  const steps: Step[] = [];
  if (!sameOrigin) steps.push("connect");
  if (needsCreds) steps.push("credentials");
  steps.push("indexer", "clients", "library", "done");
  return (
    <div className="setup-stepper" aria-hidden="true">
      {steps.map((s) => (
        <span
          key={s}
          className={"setup-dot" + (s === step ? " active" : "")}
        />
      ))}
    </div>
  );
}

// ConfiguredNotice is the "breeze past" state for a step whose section the
// daemon already has set (e.g. via .env) — a green confirmation plus a
// plain Continue, no fields to fill.
function ConfiguredNotice({
  label,
  onContinue,
}: {
  label: string;
  onContinue: () => void;
}) {
  return (
    <>
      <div className="setup-test ok">✓ {label}</div>
      <div className="setup-actions">
        <button className="setup-primary" onClick={onContinue}>
          Continue
        </button>
      </div>
    </>
  );
}

function TestLabel({ test }: { test: Test }) {
  if (test.kind === "idle") return null;
  if (test.kind === "testing")
    return (
      <div className="setup-test testing">
        <span className="coll-spinner" /> Testing…
      </div>
    );
  if (test.kind === "ok")
    return <div className="setup-test ok">✓ {test.detail}</div>;
  return <div className="setup-test err">✗ {test.detail}</div>;
}
