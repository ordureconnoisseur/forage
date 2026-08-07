import { CheckIcon, ExternalIcon } from "./icons";
import { useEffect, useRef, useState } from "react";
import {
  fetchManagedProwlarr,
  installManagedProwlarr,
  prowlarrProxyHref,
  ManagedProwlarrStatus,
} from "./api";

// ManagedProwlarrCard offers the one-click Prowlarr install and tracks it
// through download → extract → start → running. Used by the setup wizard's
// indexer step and Settings; renders nothing when managed mode isn't
// available (Docker, unsupported platform) so the manual fields stand
// alone. When the managed instance reaches running, the daemon has already
// written its URL + API key into forage's config — onReady lets the host
// view react (the wizard re-probes health and shows its "configured ✓"
// shortcut).

const POLLED_STATES = new Set(["downloading", "installing", "starting"]);

export default function ManagedProwlarrCard({
  onReady,
}: {
  onReady?: () => void;
}) {
  const [status, setStatus] = useState<ManagedProwlarrStatus | null>(null);
  const [kicking, setKicking] = useState(false);
  const readyFired = useRef(false);

  useEffect(() => {
    let closed = false;
    let timer: number | undefined;
    const poll = async () => {
      try {
        const s = await fetchManagedProwlarr();
        if (closed) return;
        setStatus(s);
        if (s.state === "running" && !readyFired.current) {
          readyFired.current = true;
          onReady?.();
        }
        if (POLLED_STATES.has(s.state)) {
          timer = window.setTimeout(poll, 1500);
        }
      } catch {
        // Endpoint unreachable — leave the card unrendered.
      }
    };
    poll();
    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kicking]);

  if (!status?.supported) return null;

  async function install() {
    setKicking(true);
    try {
      await installManagedProwlarr();
    } finally {
      // Re-arm the polling effect either way; a refused kick shows its
      // error through the status poll.
      setKicking(false);
    }
  }

  const pct =
    status.totalBytes && status.doneBytes
      ? Math.floor((status.doneBytes / status.totalBytes) * 100)
      : 0;

  return (
    <div className="managed-card">
      {(status.state === "absent" || status.state === "error") && (
        <>
          <div className="managed-line">
            <strong>No Prowlarr yet?</strong> forage can install and run it
            for you, official build, kept alongside forage&rsquo;s own data,
            managed automatically.
          </div>
          {status.state === "error" && (
            <div className="managed-err">Install failed: {status.detail}</div>
          )}
          <button className="setup-secondary small" onClick={install}>
            {status.state === "error"
              ? "Retry install"
              : "Install Prowlarr for me"}
          </button>
        </>
      )}
      {status.state === "downloading" && (
        <div className="managed-line">
          Downloading Prowlarr…{" "}
          {status.totalBytes
            ? `${pct}% of ${Math.round(status.totalBytes / 1024 / 1024)} MB`
            : ""}
          <div className="managed-progress">
            <div className="managed-progress-fill" style={{ width: pct + "%" }} />
          </div>
        </div>
      )}
      {(status.state === "installing" || status.state === "starting") && (
        <div className="managed-line">
          {status.state === "installing"
            ? "Installing…"
            : "Starting Prowlarr…"}
        </div>
      )}
      {status.state === "running" && (
        <div className="managed-line ok">
          <CheckIcon size={11} /> Managed Prowlarr {status.version} is running and connected.{" "}
          <a
            href={prowlarrProxyHref()}
            target="_blank"
            rel="noreferrer"
          >
            Open Prowlarr <ExternalIcon size={10} />
          </a>{" "}
          to add your indexers.
        </div>
      )}
      {status.state === "stopped" && status.installed && (
        <div className="managed-line">
          Managed Prowlarr {status.version} is installed but not running, it starts with the daemon.
        </div>
      )}
    </div>
  );
}
