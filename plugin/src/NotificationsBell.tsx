import { useState } from "react";
import type { NotificationCounts } from "./api";

// Header notification bell — surfaces only actionable, current-attention
// items (watches ready to grab, stalled downloads, recently-failed grabs),
// each linking to the view that resolves it. Counts are polled in App.

export default function NotificationsBell({
  counts,
  onGoWatching,
  onGoGrabs,
}: {
  counts: NotificationCounts | null;
  onGoWatching: () => void;
  onGoGrabs: () => void;
}) {
  const [open, setOpen] = useState(false);
  const c = counts ?? {
    watches_available: 0,
    grabs_stalled: 0,
    grabs_failed: 0,
  };
  const total = c.watches_available + c.grabs_stalled + c.grabs_failed;

  const items: {
    key: string;
    label: string;
    tone: string;
    go: () => void;
  }[] = [];
  if (c.watches_available > 0)
    items.push({
      key: "ready",
      tone: "accent",
      label: `${c.watches_available} ready to grab`,
      go: onGoWatching,
    });
  if (c.grabs_stalled > 0)
    items.push({
      key: "stalled",
      tone: "amber",
      label: `${c.grabs_stalled} stalled download${c.grabs_stalled > 1 ? "s" : ""}`,
      go: onGoGrabs,
    });
  if (c.grabs_failed > 0)
    items.push({
      key: "failed",
      tone: "danger",
      label: `${c.grabs_failed} failed grab${c.grabs_failed > 1 ? "s" : ""}`,
      go: onGoGrabs,
    });

  return (
    <div className="notif">
      <button
        className={"notif-bell" + (total > 0 ? " has-items" : "")}
        onClick={() => setOpen((o) => !o)}
        aria-label={total > 0 ? `${total} notifications` : "Notifications"}
        title="Notifications"
      >
        <BellIcon />
        {total > 0 && (
          <span className="notif-badge">{total > 9 ? "9+" : total}</span>
        )}
      </button>
      {open && (
        <>
          <div className="notif-scrim" onClick={() => setOpen(false)} />
          <div className="notif-panel" role="menu">
            <div className="notif-head">Notifications</div>
            {items.length === 0 ? (
              <div className="notif-empty">Nothing needs you right now.</div>
            ) : (
              items.map((it) => (
                <button
                  key={it.key}
                  className={"notif-item tone-" + it.tone}
                  onClick={() => {
                    setOpen(false);
                    it.go();
                  }}
                >
                  <span className="notif-dot" />
                  <span className="notif-label">{it.label}</span>
                  <span className="notif-arrow">→</span>
                </button>
              ))
            )}
          </div>
        </>
      )}
    </div>
  );
}

function BellIcon() {
  return (
    <svg
      viewBox="0 0 24 24"
      width="18"
      height="18"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" />
      <path d="M13.73 21a2 2 0 0 1-3.46 0" />
    </svg>
  );
}
