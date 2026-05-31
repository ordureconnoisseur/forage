import { useState } from "react";
import {
  adminToken,
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
// forage has two setup layers Hearth doesn't: (1) Connect — point the
// plugin at the daemon (URL + token if required), browser-side, probed
// via the public /healthz; (2) Credentials — when /healthz reports the
// daemon is unconfigured, collect the minimum Stash + StashDB creds and
// POST them to /config. Step 2 is skipped when the daemon already has
// credentials (e.g. set via .env).

type Step = "welcome" | "connect" | "credentials" | "done";
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

export default function Setup({
  health,
  healthError,
  onDone,
  onAdvanced,
}: {
  health: Health | null;
  healthError: string | null;
  // Bump App's health probe so it re-evaluates needsSetup and unmounts
  // the wizard once everything's configured.
  onDone: () => void;
  // Escape hatch to the full Settings panel for anything the wizard
  // doesn't cover (Prowlarr, download clients, advanced).
  onAdvanced: () => void;
}) {
  // Jump straight to the relevant step when there's already partial
  // setup: unreachable URL → reconnect; reachable but unconfigured →
  // credentials; otherwise start from the welcome screen.
  const initialStep: Step = !foragerBase()
    ? "welcome"
    : healthError
      ? "connect"
      : health?.unconfigured
        ? "credentials"
        : "welcome";

  const [step, setStep] = useState<Step>(initialStep);

  // Connect step
  const [url, setUrl] = useState(foragerBase());
  const [token, setToken] = useState(adminToken());
  const [needsToken, setNeedsToken] = useState(!!health?.adminAuthRequired);
  const [connTest, setConnTest] = useState<Test>({ kind: "idle" });
  const [connHealth, setConnHealth] = useState<Health | null>(health);

  // Credentials step
  const [stashUrl, setStashUrl] = useState("");
  const [stashKey, setStashKey] = useState("");
  const [stashdbUrl, setStashdbUrl] = useState("https://stashdb.org");
  const [stashdbKey, setStashdbKey] = useState("");
  const [stashTest, setStashTest] = useState<Test>({ kind: "idle" });
  const [stashdbTest, setStashdbTest] = useState<Test>({ kind: "idle" });
  const [credErr, setCredErr] = useState<string | null>(null);
  const [savingCreds, setSavingCreds] = useState(false);

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
    setConnHealth(h);
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
    // Skip the credentials step when the daemon is already configured.
    if (connHealth && !connHealth.unconfigured) {
      finish();
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
      finish();
    } catch (e) {
      setCredErr((e as Error).message);
    } finally {
      setSavingCreds(false);
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
          <Stepper step={step} needsCreds={!!connHealth?.unconfigured} />
        )}

        {step === "welcome" && (
          <div className="setup-welcome">
            <AcornIcon className="setup-acorn" />
            <h1>forage</h1>
            <p className="setup-tagline">
              Performer-driven scene grabbing for Stash. Let's point the plugin
              at your daemon.
            </p>
            <button
              className="setup-primary"
              onClick={() => setStep("connect")}
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
              key to know each performer's filmography. You can add Prowlarr and
              download clients later in Settings.
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
                {savingCreds ? "Saving…" : "Finish setup"}
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

function Stepper({ step, needsCreds }: { step: Step; needsCreds: boolean }) {
  // welcome isn't a dot; show connect → (credentials) → done.
  const steps: Step[] = needsCreds
    ? ["connect", "credentials", "done"]
    : ["connect", "done"];
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
