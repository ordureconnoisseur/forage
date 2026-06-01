import { useState } from "react";
import { adminToken, clearSession, establishSession, setAdminToken, verifyToken } from "../api";
import AcornIcon from "../AcornIcon";

// Login gate — shown when the daemon reports adminAuthRequired but this
// browser doesn't hold a valid token (the *arr login page). Distinct from
// the setup wizard: the wizard configures an *unconfigured* daemon; this
// unlocks a *configured but locked* one. Reuses the .setup-* styling so it
// looks of a piece with onboarding.
//
// On submit we store the token, establish the forage_token cookie (so
// <img> loads authenticate), then verify against a gated endpoint before
// declaring success — a stored-but-wrong token stays on the gate with an
// inline "Token rejected" message rather than entering a 401-ing app.
export default function Login({ onAuthed }: { onAuthed: () => void }) {
  const [token, setToken] = useState(adminToken());
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function submit() {
    const t = token.trim();
    if (!t) {
      setErr("Enter your admin token");
      return;
    }
    setBusy(true);
    setErr(null);
    setAdminToken(t);
    try {
      await establishSession();
      if (await verifyToken()) {
        onAuthed();
      } else {
        // Reject cleanly: drop the bad token + cookie so a reload doesn't
        // resurrect a half-authenticated state.
        setAdminToken("");
        await clearSession();
        setErr("Token rejected — check it and try again");
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
            🔒 This forage is locked. Enter your admin token to continue.
          </p>
        </div>
        <div className="setup-step">
          <label className="setup-field">
            <span>Admin token</span>
            <input
              type="password"
              value={token}
              spellCheck={false}
              autoComplete="off"
              autoFocus
              placeholder="admin token"
              onChange={(e) => setToken(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && !busy && submit()}
            />
          </label>
          {err && <div className="setup-err">{err}</div>}
          <div className="setup-actions">
            <button
              className="setup-primary"
              onClick={submit}
              disabled={busy || !token.trim()}
            >
              {busy ? "Checking…" : "Log in"}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
