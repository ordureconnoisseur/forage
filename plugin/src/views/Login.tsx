import { useState } from "react";
import {
  adminToken,
  clearSession,
  establishSession,
  login,
  setAdminToken,
  verifyToken,
} from "../api";
import { clearCache } from "../swr";
import AcornIcon from "../AcornIcon";

// Login gate — shown when the daemon reports adminAuthRequired but this
// browser isn't authenticated. Distinct from the setup wizard: the wizard
// configures an *unconfigured* daemon; this unlocks a *configured but
// locked* one. Reuses the .setup-* styling so it looks of a piece with
// onboarding.
//
// Two login paths, matching the daemon's two-credential model:
//   - Username + password (the default; `passwordSet`) → POST /login,
//     which sets the forage_token session cookie. On success we're authed
//     by that cookie — no localStorage involved.
//   - API key (the admin token) → the fallback for a token-only daemon, or
//     for a client that only holds the key. Stores the token, establishes
//     the cookie, and verifies against a gated endpoint before entering.
// loginError turns a failed sign-in into something true.
//
// A 429 used to land here as "Incorrect username or password", because
// any error carrying a status was rendered as that. The daemon has no
// lockout by design, but a user who mistyped a few times and then typed
// the right password would be told their password was wrong, for minutes,
// with no mention of waiting: indistinguishable from the lockout that
// deliberately does not exist.
function loginError(e: unknown): string {
  const status = (e as Error & { status?: number }).status;
  if (status === 429) {
    return (
      (e as Error).message ||
      "Too many attempts just now. Wait a moment and try again."
    );
  }
  if (status) return "Incorrect username or password";
  return "Couldn't reach the daemon, try again";
}

export default function Login({
  onAuthed,
  passwordSet,
}: {
  onAuthed: () => void;
  passwordSet: boolean;
}) {
  // Default to the password form when the daemon has one; otherwise the
  // only way in is the API key.
  const [mode, setMode] = useState<"password" | "apikey">(
    passwordSet ? "password" : "apikey",
  );
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState(adminToken());
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function submitPassword() {
    if (!username.trim() || !password) {
      setErr("Enter your username and password");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await login(username.trim(), password);
      // The session cookie is set; we're in.
      onAuthed();
    } catch (e) {
      setErr(loginError(e));
    } finally {
      setBusy(false);
    }
  }

  async function submitToken() {
    const t = token.trim();
    if (!t) {
      setErr("Enter your API key");
      return;
    }
    setBusy(true);
    setErr(null);
    setAdminToken(t);
    try {
      const status = await establishSession();
      if (status === 429) {
        // Don't drop the key: it may well be right, and the daemon just
        // isn't answering credential checks from here for a minute.
        setErr("Too many attempts just now. Wait a moment and try again.");
        return;
      }
      if (await verifyToken()) {
        onAuthed();
      } else {
        // Reject cleanly: drop the bad key + cookie so a reload doesn't
        // resurrect a half-authenticated state.
        setAdminToken("");
        await clearSession();
        clearCache();
        setErr("API key rejected — check it and try again");
      }
    } catch {
      setErr("Couldn't reach the daemon — try again");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="setup">
      <div className="setup-card">
        <div className="setup-welcome">
          <AcornIcon className="setup-acorn" />
          <h1>forage</h1>
          <p className="setup-tagline">
            🔒 This forage is locked. {mode === "password"
              ? "Sign in to continue."
              : "Enter your API key to continue."}
          </p>
        </div>
        <div className="setup-step">
          {mode === "password" ? (
            <>
              <label className="setup-field">
                <span>Username</span>
                <input
                  type="text"
                  value={username}
                  spellCheck={false}
                  autoComplete="username"
                  autoFocus
                  placeholder="username"
                  onChange={(e) => setUsername(e.target.value)}
                  onKeyDown={(e) =>
                    e.key === "Enter" && !busy && submitPassword()
                  }
                />
              </label>
              <label className="setup-field">
                <span>Password</span>
                <input
                  type="password"
                  value={password}
                  spellCheck={false}
                  autoComplete="current-password"
                  placeholder="password"
                  onChange={(e) => setPassword(e.target.value)}
                  onKeyDown={(e) =>
                    e.key === "Enter" && !busy && submitPassword()
                  }
                />
              </label>
              {err && <div className="setup-err">{err}</div>}
              <div className="setup-actions">
                <button
                  className="setup-primary"
                  onClick={submitPassword}
                  disabled={busy || !username.trim() || !password}
                >
                  {busy ? "Signing in…" : "Sign in"}
                </button>
              </div>
              <button
                type="button"
                className="setup-link"
                onClick={() => {
                  setErr(null);
                  setMode("apikey");
                }}
              >
                Use an API key instead
              </button>
            </>
          ) : (
            <>
              <label className="setup-field">
                <span>API key</span>
                <input
                  type="password"
                  value={token}
                  spellCheck={false}
                  autoComplete="off"
                  autoFocus
                  placeholder="API key"
                  onChange={(e) => setToken(e.target.value)}
                  onKeyDown={(e) => e.key === "Enter" && !busy && submitToken()}
                />
              </label>
              {err && <div className="setup-err">{err}</div>}
              <div className="setup-actions">
                <button
                  className="setup-primary"
                  onClick={submitToken}
                  disabled={busy || !token.trim()}
                >
                  {busy ? "Checking…" : "Log in"}
                </button>
              </div>
              {passwordSet && (
                <button
                  type="button"
                  className="setup-link"
                  onClick={() => {
                    setErr(null);
                    setMode("password");
                  }}
                >
                  Sign in with username + password
                </button>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
}
