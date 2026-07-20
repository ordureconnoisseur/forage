import { useEffect, useState } from "react";
import {
  type Subscription,
  fetchSubscriptions,
  unsubscribe,
  setSubscriptionAutoGrab,
  markSubscriptionSeen,
  performerImageURL,
  studioImageURL,
} from "./api";

// Line icons in the app's stroke style (24 viewBox, currentColor,
// round caps), same language as WatchControl's bookmark. No emoji:
// glyph rendering varies wildly across platforms; these don't.
function iconProps(size: number) {
  return {
    viewBox: "0 0 24 24",
    width: size,
    height: size,
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 2,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true,
  };
}

// Bolt outline (SVG Repo, CC0): the auto-grab toggle.
function BoltIcon() {
  return (
    <svg {...iconProps(12)}>
      <path d="M12.9996 3L5.06859 12.6934C4.72703 13.1109 4.55625 13.3196 4.55471 13.4956C4.55336 13.6486 4.62218 13.7939 4.74148 13.8897C4.87867 14 5.14837 14 5.68776 14H11.9996L10.9996 21L18.9305 11.3066C19.2721 10.8891 19.4429 10.6804 19.4444 10.5044C19.4458 10.3514 19.377 10.2061 19.2577 10.1103C19.1205 10 18.8508 10 18.3114 10H11.9996L12.9996 3Z" />
    </svg>
  );
}

function XIcon() {
  return (
    <svg {...iconProps(11)}>
      <path d="M6 6l12 12M18 6L6 18" />
    </svg>
  );
}

// Notification bell: the "new scenes found" badge.
function BellIcon() {
  return (
    <svg {...iconProps(10)} strokeWidth={2.4}>
      <path d="M18 15.5V11a6 6 0 1 0-12 0v4.5L4.5 18h15L18 15.5z" />
      <path d="M10 21a2.2 2.2 0 0 0 4 0" />
    </svg>
  );
}

function DownIcon() {
  return (
    <svg {...iconProps(10)} strokeWidth={2.6}>
      <path d="M12 4v14M6 12l6 6 6-6" />
    </svg>
  );
}

// Card portrait: prefer the daemon's Stash image proxy by local id (the
// same images the Performers/Studios grids show), then any stored
// image_url, then the initial-letter placeholder.
function subImageURL(sub: Subscription): string | null {
  if (sub.local_id) {
    const proxied =
      sub.kind === "studio"
        ? studioImageURL(sub.local_id)
        : performerImageURL(sub.local_id);
    if (proxied) return proxied;
  }
  return sub.image_url || null;
}

// SubscriptionsRow: the permanent watches for ONE subject kind, as a
// pinned card row at the top of the Performers/Studios browse tabs
// (subscriptions are standing curation about subjects, so they live
// with the subjects; the Grabs tab stays a pure activity feed). Each card badges activity since the user
// last opened it (new watches created by the subscription loop) plus a
// ready count (watches sitting available). Clicking a card marks it seen
// and jumps to the subject's scenes view. The lightning toggle flips
// hands-free auto-grab for that subscription.
export default function SubscriptionsRow({ kind }: { kind: "performer" | "studio" }) {
  const [subs, setSubs] = useState<Subscription[]>([]);
  const [err, setErr] = useState<string | null>(null);

  const load = () =>
    fetchSubscriptions()
      .then((r) => {
        setSubs(r.subscriptions.filter((x) => x.kind === kind));
        setErr(null);
      })
      .catch((e) => setErr((e as Error).message));

  useEffect(() => {
    load();
    const id = window.setInterval(load, 60_000);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind]);

  if (err || subs.length === 0) return null;

  const openSubject = (sub: Subscription) => {
    void markSubscriptionSeen(sub.stashdb_id).finally(load);
    // Performer pages navigate by LOCAL Stash id; studio pages by
    // cross-id. Legacy rows without local_id keep the stored key (they
    // predate the rekey and hold the local id there anyway).
    window.location.hash =
      sub.kind === "studio"
        ? `#/studio/${encodeURIComponent(sub.stashdb_id)}`
        : `#/performer/${sub.local_id || sub.stashdb_id}`;
  };

  return (
    <div className="subs-rail">
      <div className="subs-rail-head">
        <h3>Subscribed</h3>
        <span className="subs-rail-hint">
          new scenes are watched automatically
        </span>
      </div>
      <ul className="subs-cards">
        {subs.map((sub) => (
          <li
            key={sub.stashdb_id}
            className={"subs-card" + (sub.auto_grab ? " auto-on" : "")}
          >
            <button
              className="subs-card-body"
              onClick={() => openSubject(sub)}
              title={`Open ${sub.name}`}
            >
              {subImageURL(sub) ? (
                <img
                  src={subImageURL(sub)!}
                  alt=""
                  loading="lazy"
                  onError={(e) => {
                    (e.currentTarget as HTMLImageElement).style.display =
                      "none";
                  }}
                />
              ) : (
                <span className="subs-card-initial">
                  {sub.name.slice(0, 1).toUpperCase()}
                </span>
              )}
              {sub.new_count > 0 && (
                <span
                  className="subs-badge"
                  title={`${sub.new_count} new scene${sub.new_count === 1 ? "" : "s"} found since you last looked`}
                >
                  <BellIcon />
                  {sub.new_count > 99 ? "99+" : sub.new_count}
                </span>
              )}
              {sub.ready_count > 0 && (
                <span
                  className="subs-badge subs-badge-ready"
                  title={`${sub.ready_count} release${sub.ready_count === 1 ? "" : "s"} ready to grab`}
                >
                  <DownIcon />
                  {sub.ready_count}
                </span>
              )}
              <span className="subs-scrim">
                <span className="subs-card-name">{sub.name}</span>
              </span>
            </button>
            <div className="subs-card-actions">
              <button
                className={"subs-auto" + (sub.auto_grab ? " on" : "")}
                title={
                  sub.auto_grab
                    ? "Auto-grab ON: available releases are grabbed hands-free"
                    : "Auto-grab OFF: you get a ping with a Grab button"
                }
                onClick={() =>
                  setSubscriptionAutoGrab(sub.stashdb_id, !sub.auto_grab).then(load)
                }
              >
                <BoltIcon />
              </button>
              <button
                className="subs-remove"
                title="Unsubscribe (existing watches are kept)"
                onClick={() => unsubscribe(sub.stashdb_id).then(load)}
              >
                <XIcon />
              </button>
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}
