import { useEffect, useState } from "react";
import {
  type Subscription,
  fetchSubscriptions,
  markSubscriptionSeen,
  performerImageURL,
  studioImageURL,
} from "./api";
import { BellIcon } from "./icons";

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
// pinned card row at the top of the Performers/Studios browse tabs.
// Deliberately glance-only: portrait, name, and AT MOST one badge per
// card (activity since last look; green once something is grabbable).
// Managing a subscription (auto-grab, unsubscribe) lives on the
// subject's own page next to the Subscribe button, so the tiny cards
// carry no controls. Clicking a card marks it seen and opens the
// subject. A thin gold baseline marks auto-grab subscriptions.
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

  // One badge per card. Ready-to-grab beats "new activity": it is the
  // state you can act on, and stacking both counts on a 96px tile was
  // noise. The tooltip carries the detail.
  const badge = (sub: Subscription) => {
    if (sub.ready_count > 0) {
      return (
        <span
          className="subs-badge subs-badge-ready"
          title={`${sub.ready_count} release${sub.ready_count === 1 ? "" : "s"} ready to grab`}
        >
          <BellIcon />
          {sub.ready_count}
        </span>
      );
    }
    if (sub.new_count > 0) {
      return (
        <span
          className="subs-badge"
          title={`${sub.new_count} new scene${sub.new_count === 1 ? "" : "s"} found since you last looked`}
        >
          <BellIcon />
          {sub.new_count > 99 ? "99+" : sub.new_count}
        </span>
      );
    }
    return null;
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
              title={
                `Open ${sub.name}` +
                (sub.auto_grab ? " (auto-grab is on)" : "")
              }
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
              {badge(sub)}
              <span className="subs-scrim">
                <span className="subs-card-name">{sub.name}</span>
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}
