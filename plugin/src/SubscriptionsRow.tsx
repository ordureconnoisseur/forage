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
                <span className="subs-badge" title={`${sub.new_count} new since you last looked`}>
                  {sub.new_count > 99 ? "99+" : sub.new_count}
                </span>
              )}
              {sub.ready_count > 0 && (
                <span
                  className="subs-badge subs-badge-ready"
                  title={`${sub.ready_count} release${sub.ready_count === 1 ? "" : "s"} ready to grab`}
                >
                  ⬇{sub.ready_count}
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
                ⚡
              </button>
              <button
                className="subs-remove"
                title="Unsubscribe (existing watches are kept)"
                onClick={() => unsubscribe(sub.stashdb_id).then(load)}
              >
                ✕
              </button>
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}
