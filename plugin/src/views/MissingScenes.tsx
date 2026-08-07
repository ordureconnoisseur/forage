import { useEffect, useRef, useState } from "react";
import {
  addWatches,
  destroyScene,
  fetchMissing,
  type AddWatchReq,
  type DuplicateScene,
  type MissingResponse,
  type MissingScene,
  type OwnedScene,
  fetchSubscriptions,
  subscribe,
  unsubscribe,
  setSubscriptionAutoGrab,
  createUpgradeWatches,
} from "../api";
import { humanSize } from "../format";
import { ArrowDownIcon, ArrowUpIcon, BoltIcon, CheckIcon, ClockIcon, ExternalIcon, RefreshIcon, StarIcon } from "../icons";
import WatchControl from "../WatchControl";
import PerformerPacks from "./PerformerPacks";

// SceneView is the Owned · Both · Missing · Dupes filter on the performer page.
type SceneView = "owned" | "both" | "missing" | "dupes";

// resTierFromLabel maps a resolution label ("480p"/"1080p"/"2160p") to a
// quality tier class, so the owned badge can tint low-res scenes (the best
// upgrade candidates) warm and high-res ones cool.
function resTierFromLabel(label?: string): string {
  if (!label) return "";
  const n = parseInt(label, 10) || 0;
  if (n >= 2160) return "uhd";
  if (n >= 1080) return "fhd";
  if (n >= 720) return "hd";
  return "sd";
}

// grabStatusLabel maps a raw grab status to a short card badge.
//
// Returns a node rather than a string because the leading glyph was an
// hourglass and three arrows typed as characters, which render in whatever
// fallback face the platform picks and are emoji on some of them. A string
// return type is what forced that; the badge is one JSX slot, so it can hold
// an icon just as easily.
function grabStatusLabel(status: string) {
  switch (status) {
    case "queued":
      return (
        <>
          <ClockIcon size={10} /> Queued
        </>
      );
    case "downloading":
      return (
        <>
          <ArrowDownIcon size={10} /> Downloading
        </>
      );
    case "completed":
    case "placed":
      return (
        <>
          <ArrowDownIcon size={10} /> Downloaded
        </>
      );
    case "scanned":
      return (
        <>
          <RefreshIcon size={10} /> Scanning
        </>
      );
    default:
      return (
        <>
          <ArrowDownIcon size={10} /> In progress
        </>
      );
  }
}

export default function MissingScenes({
  subject,
  onPickScene,
}: {
  // Who/what this page is about. A performer page and a studio page share the
  // entire view; they differ only in placement (see placementFor below).
  subject: { kind: "performer" | "studio"; id: string };
  // Receives the placement performer name too so the scene-releases page can
  // pass it through to /grab — the placer needs to know which library folder
  // to drop the file in.
  onPickScene: (stashDBID: string, performerName: string) => void;
}) {
  const [data, setData] = useState<MissingResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  // Multi-select mode: when on, cards toggle selection instead of
  // navigating. selected holds the chosen StashDB ids.
  const [selecting, setSelecting] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [watchBusy, setWatchBusy] = useState(false);
  // Subscription state for the header button: whether THIS subject has a
  // permanent watch. Loaded once per subject; toggled optimistically.
  const [subscribed, setSubscribed] = useState(false);
  const [subAuto, setSubAuto] = useState(false);
  const [subBusy, setSubBusy] = useState(false);
  const [upgBusy, setUpgBusy] = useState(false);
  const [upgMsg, setUpgMsg] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    setSubscribed(false);
    setSubAuto(false);
    fetchSubscriptions()
      .then((r) => {
        if (cancelled) return;
        // subject.id is the page's navigation id: LOCAL Stash id for
        // performers, StashDB cross-id for studios. Subscriptions key on
        // the cross-id and carry local_id, so match either.
        const match = r.subscriptions.find(
          (x) => x.stashdb_id === subject.id || x.local_id === subject.id,
        );
        setSubscribed(!!match);
        setSubAuto(!!match?.auto_grab);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [subject.kind, subject.id]);
  const [watchedMsg, setWatchedMsg] = useState<string | null>(null);
  // Owned · Both · Missing filter. Persisted so the choice sticks across
  // performers and sessions; defaults to Missing (the original behaviour).
  const [view, setView] = useState<SceneView>(() => {
    const v = localStorage.getItem("forage.sceneView");
    return v === "owned" || v === "both" || v === "dupes" ? v : "missing";
  });
  // Bumped after a duplicate-copy delete to re-fetch (the deleted copy
  // disappears, and a scene with one copy left drops off the Dupes list).
  const [reloadKey, setReloadKey] = useState(0);
  useEffect(() => {
    localStorage.setItem("forage.sceneView", view);
    // Multi-select is a missing-only action — drop it when leaving that view.
    setSelecting(false);
    setSelected(new Set());
  }, [view]);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setData(null);
    setError(null);
    // Reset selection when switching subjects.
    setSelecting(false);
    setSelected(new Set());
    fetchMissing(
      subject.kind === "studio"
        ? { studio: subject.id }
        : { performer: subject.id },
    )
      .then((r) => {
        if (cancelled) return;
        setData(r);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(e.message);
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [subject.kind, subject.id, reloadKey]);

  if (loading) return <div className="empty">Loading missing scenes…</div>;
  if (error) return <div className="empty error">Failed to load: {error}</div>;
  if (!data) return null;

  const isStudio = subject.kind === "studio";
  // Stable per-subject batch: every watch added from this page, one by
  // one via the card control or via multi-select, joins the same
  // Watching-tab group, labeled with the subject's name. Deterministic
  // (kind + id) so repeat visits and repeat selections keep merging.
  const subjectBatch = {
    id: `${subject.kind}:${subject.id}`,
    label: data.subject.name,
  };
  // The library folder a grab/watch for a given scene lands in. A performer
  // page uses the page performer; a studio is not a folder, so a studio page
  // uses each scene's own primary performer (per-scene). performer_id is only
  // available for the performer-page case.
  const placementFor = (s: { performers?: { name: string }[] }) =>
    isStudio
      ? { name: s.performers?.[0]?.name ?? "", id: "" }
      : { name: data.subject.name, id: data.subject.local_id };

  const toggleSelected = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const exitSelect = () => {
    setSelecting(false);
    setSelected(new Set());
  };

  // Watch a set of missing scenes as ONE batch — the Watching tab groups them
  // under the performer's name and shows their collective progress. Used by
  // both "watch selected" and "watch all missing". No quality target (the
  // matcher surfaces the best release by your preference ranking).
  const watchScenes = async (scenes: MissingScene[]) => {
    if (scenes.length === 0) return;
    setWatchBusy(true);
    const watches: AddWatchReq[] = scenes.map((s) => {
      const place = placementFor(s);
      return {
        stashdb_id: s.stashdb_id,
        title: s.title,
        date: s.date,
        studio: s.studio,
        image_url: s.image_url,
        performer_name: place.name,
        performer_id: place.id,
        performers: (s.performers || []).map((p) => p.name),
      };
    });
    try {
      await addWatches({
        batch_id: subjectBatch.id,
        batch_label: subjectBatch.label,
        watches,
      });
    } catch {
      // best-effort; the toast still confirms intent
    }
    setWatchBusy(false);
    const n = scenes.length;
    exitSelect();
    setWatchedMsg(`Watching ${n} scene${n === 1 ? "" : "s"}`);
    window.setTimeout(() => setWatchedMsg(null), 3500);
  };

  // Selection works in the Missing view only (Watch a hand-picked subset).
  // Owned/Both have no bulk action now that upgrades are gone.
  const selectable = view === "missing";
  const allIds = data.missing.map((s) => s.stashdb_id);
  const allSelected = selected.size === allIds.length && allIds.length > 0;

  // The cards to show for the current view. Owned scenes are tagged so the
  // card can show a resolution badge and route to the upgrade (release-search)
  // flow; missing scenes behave as before.
  type Entry =
    | { kind: "owned"; s: OwnedScene }
    | { kind: "missing"; s: MissingScene };
  let entries: Entry[];
  if (view === "owned") {
    // Worst quality first — those are the best upgrade candidates.
    entries = [...data.owned]
      .sort((a, b) => (a.height ?? 0) - (b.height ?? 0))
      .map((s) => ({ kind: "owned", s }));
  } else if (view === "both") {
    // The whole filmography, newest first, each badged owned/missing.
    entries = [
      ...data.owned.map((s) => ({ kind: "owned" as const, s })),
      ...data.missing.map((s) => ({ kind: "missing" as const, s })),
    ].sort((a, b) => (b.s.date ?? "").localeCompare(a.s.date ?? ""));
  } else {
    entries = data.missing.map((s) => ({ kind: "missing", s }));
  }
  const subjectNoun = isStudio ? "studio" : "performer";
  const emptyMessage =
    view === "owned"
      ? `You don't own any of this ${subjectNoun}'s StashDB scenes yet.`
      : view === "both"
        ? `No StashDB scenes found for this ${subjectNoun}.`
        : `You have every StashDB scene for this ${subjectNoun} in your library.`;

  return (
    <div>
      <div className="page-header page-header-row">
        <div>
          <h2>
            {data.subject.name}
            <button
              className={"subscribe-btn" + (subscribed ? " on" : "")}
              disabled={subBusy}
              title={
                subscribed
                  ? "Subscribed: new scenes are watched automatically. Click to unsubscribe."
                  : "Subscribe: automatically watch every scene released from today on. Backlog stays manual (Watch all missing below)."
              }
              onClick={() => {
                setSubBusy(true);
                // Subscriptions key on the StashDB cross-id (the loop
                // matches scenes by it); the local id rides along for
                // card navigation + the portrait image proxy.
                const fn = subscribed
                  ? unsubscribe(data.subject.stashdb_id)
                  : subscribe({
                      stashdb_id: data.subject.stashdb_id,
                      kind: subject.kind,
                      name: data.subject.name,
                      local_id: data.subject.local_id,
                    });
                fn.then(() => {
                  setSubscribed(!subscribed);
                  if (subscribed) setSubAuto(false);
                })
                  .catch(() => {})
                  .finally(() => setSubBusy(false));
              }}
            >
              {subscribed ? (
            <>
              <StarIcon size={11} filled /> Subscribed
            </>
          ) : (
            <>
              <StarIcon size={11} /> Subscribe
            </>
          )}
            </button>
            {subscribed && (
              <button
                className={"subscribe-btn auto" + (subAuto ? " on" : "")}
                title={
                  subAuto
                    ? "Auto-grab ON: new releases are grabbed hands-free. Click to switch to pings."
                    : "Auto-grab OFF: you get a notification with a Grab button for each new release. Click to grab hands-free."
                }
                onClick={() => {
                  const next = !subAuto;
                  setSubAuto(next);
                  setSubscriptionAutoGrab(data.subject.stashdb_id, next).catch(
                    () => setSubAuto(!next),
                  );
                }}
              >
                <span className="btn-ico">
                  <BoltIcon size={11} />
                </span>
                Auto
              </button>
            )}
            <button
              className="subscribe-btn"
              disabled={upgBusy}
              title="Create upgrade watches: every owned scene below 1080p gets watched for a better release. When one lands, the old and new copies go to the duplicate review queue."
              onClick={() => {
                setUpgBusy(true);
                createUpgradeWatches({
                  kind: subject.kind,
                  stashdb_id: data.subject.stashdb_id,
                  name: data.subject.name,
                  cutoff: 1080,
                })
                  .then((r) =>
                    setUpgMsg(
                      r.created > 0
                        ? `watching ${r.created} scene${r.created === 1 ? "" : "s"} for upgrades`
                        : "nothing below 1080p to upgrade",
                    ),
                  )
                  .catch((e) => setUpgMsg((e as Error).message))
                  .finally(() => setUpgBusy(false));
              }}
            >
              {upgBusy ? (
            "…"
          ) : (
            <>
              <ArrowUpIcon size={11} /> Upgrades &lt;1080p
            </>
          )}
            </button>
            {upgMsg && <span className="upg-msg">{upgMsg}</span>}
          </h2>
          <div className="meta">
            {data.total_scenes} on StashDB · {data.owned_count} in library ·{" "}
            <strong>{data.missing.length} missing</strong>
            {data.unidentified_local > 0 && (
              <span
                className="ms-unidentified"
                title="Local scenes tagged with this name that carry no StashDB id, so they aren't matched against StashDB's catalogue. Some of what shows as 'missing' may already be here, just un-identified — run Stash's identify/scrape to fold them in."
              >
                {" · "}+{data.unidentified_local.toLocaleString()} un-identified
              </span>
            )}
          </div>
          <div className="scene-view-toggle" role="tablist" aria-label="Scene filter">
            {(
              [
                ["missing", "Missing", data.missing.length],
                ["both", "Both", data.total_scenes],
                ["owned", "Owned", data.owned.length],
                ["dupes", "Dupes", data.duplicates.length],
              ] as [SceneView, string, number][]
            ).map(([v, label, count]) => (
              <button
                key={v}
                role="tab"
                aria-selected={view === v}
                className={"scene-view-tab" + (view === v ? " active" : "")}
                onClick={() => setView(v)}
              >
                {label}
                <span className="scene-view-count">{count}</span>
              </button>
            ))}
          </div>
        </div>
        {selectable &&
          allIds.length > 0 &&
          (selecting ? (
            <div className="ms-select-actions">
              <button
                className="ms-select-toggle"
                onClick={() =>
                  setSelected(allSelected ? new Set() : new Set(allIds))
                }
              >
                {allSelected ? "Clear all" : "Select all"}
              </button>
              <button className="ms-select-cancel" onClick={exitSelect}>
                Cancel
              </button>
            </div>
          ) : (
            <div className="ms-select-actions">
              <button
                className="ms-select-toggle"
                onClick={() => setSelecting(true)}
              >
                Select
              </button>
              <button
                className="collection-cta"
                disabled={watchBusy}
                onClick={() => watchScenes(data.missing)}
                title="Watch every missing scene for releases (one batch)"
              >
                {watchBusy ? "Watching…" : "Watch all missing"}
              </button>
            </div>
          ))}
      </div>
      {view === "dupes" ? (
        data.duplicates.length === 0 ? (
          <div className="empty">
            No duplicates — you hold a single copy of every owned scene.
          </div>
        ) : (
          <div className="dup-list">
            {data.duplicates.map((d) => (
              <DuplicateCard
                key={d.stashdb_id}
                dup={d}
                onDeleted={() => setReloadKey((k) => k + 1)}
              />
            ))}
          </div>
        )
      ) : entries.length === 0 ? (
        <div className="empty">{emptyMessage}</div>
      ) : (
        <div className="scene-grid">
          {entries.map((e) => {
            const place = placementFor(e.s);
            return (
              <SceneCard
                key={e.s.stashdb_id}
                s={e.s}
                performerName={place.name}
                performerId={place.id}
                batch={subjectBatch}
                selecting={selecting}
                selected={selected.has(e.s.stashdb_id)}
                owned={e.kind === "owned"}
                resolution={e.kind === "owned" ? e.s.resolution : undefined}
                onPick={() =>
                  selecting
                    ? toggleSelected(e.s.stashdb_id)
                    : onPickScene(e.s.stashdb_id, place.name)
                }
              />
            );
          })}
        </div>
      )}
      {view === "missing" && selecting && (
        <div className="ms-select-bar">
          <span className="ms-select-count">{selected.size} selected</span>
          <button
            className="ms-select-watch"
            disabled={selected.size === 0 || watchBusy}
            onClick={() =>
              watchScenes(
                data.missing.filter((s) => selected.has(s.stashdb_id)),
              )
            }
            title="Watch all selected scenes for releases (one batch)"
          >
            {watchBusy ? "Watching…" : `Watch ${selected.size} selected`}
          </button>
        </div>
      )}
      {!isStudio && view !== "dupes" && data.subject.local_id && (
        <PerformerPacks
          performerId={data.subject.local_id}
          performerName={data.subject.name}
        />
      )}
      {watchedMsg && <div className="ms-toast">{watchedMsg}</div>}
    </div>
  );
}

function SceneCard({
  s,
  performerName,
  performerId,
  batch,
  selecting,
  selected,
  owned,
  resolution,
  onPick,
}: {
  s: MissingScene;
  performerName: string;
  performerId: string;
  batch: { id: string; label?: string };
  selecting: boolean;
  selected: boolean;
  // owned marks an already-in-library scene: show its current resolution and
  // route clicks to the upgrade (release-search) flow instead of offering a
  // Watch control (you don't watch what you already have).
  owned?: boolean;
  resolution?: string;
  onPick: () => void;
}) {

  return (
    <div
      className={
        "scene-card" +
        (selecting ? " selectable" : "") +
        (selected ? " selected" : "") +
        (owned ? " owned" : "")
      }
      role="button"
      tabIndex={0}
      onClick={onPick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") onPick();
      }}
      aria-pressed={selecting ? selected : undefined}
      title={owned ? `In your library at ${resolution || "unknown quality"} — click to find a better release` : undefined}
    >
      {selecting && (
        <span className="scene-check" aria-hidden="true">
          {selected ? <CheckIcon size={11} /> : null}
        </span>
      )}
      <div className="scene-thumb">
        {s.image_url ? (
          <img
            src={s.image_url}
            alt=""
            loading="lazy"
            onLoad={(e) => {
              // Same contract as Discover: stop the placeholder once the image
              // is there. Without this every card on the page keeps an infinite
              // CSS animation repainting behind a picture that already arrived.
              (e.currentTarget as HTMLImageElement)
                .parentElement?.classList.add("thumb-loaded");
            }}
            onError={(e) => {
              const img = e.currentTarget as HTMLImageElement;
              img.style.display = "none";
              // A thumbnail that failed is not still loading.
              img.parentElement?.classList.add("thumb-failed");
            }}
          />
        ) : null}
        <a
          href={`https://stashdb.org/scenes/${s.stashdb_id}`}
          target="_blank"
          rel="noopener noreferrer"
          className="thumb-external"
          title="Open on StashDB"
          onClick={(e) => e.stopPropagation()}
        >
          <ExternalIcon size={11} />
        </a>
        {owned && resolution && (
          <span
            className={"scene-res-badge res-" + resTierFromLabel(resolution)}
            title={`You have this at ${resolution}`}
          >
            {resolution}
          </span>
        )}
        {s.grab_status && (
          <span
            className={"scene-grab-badge gs-" + s.grab_status}
            title={`Grab in progress — ${s.grab_status}`}
          >
            {grabStatusLabel(s.grab_status)}
          </span>
        )}
        {/* Watch control — hidden in select mode (the card toggles
            selection then) and for owned scenes (nothing to wait for). */}
        {!selecting && !owned && (
          <WatchControl
            scene={{
              stashdb_id: s.stashdb_id,
              title: s.title,
              date: s.date,
              studio: s.studio,
              image_url: s.image_url,
            }}
            performerName={performerName}
            performerId={performerId}
            batch={batch}
            initialStatus={s.watch_status || ""}
            variant="overlay"
          />
        )}
      </div>
      <div className="scene-info">
        <div className="title">{s.title || "(untitled)"}</div>
        <div className="meta">
          {s.date && <span>{s.date}</span>}
          {s.date && s.studio && <span> · </span>}
          {s.studio && <span>{s.studio}</span>}
        </div>
        {s.performers && s.performers.length > 0 ? (
          <div className="performers">
            {s.performers
              .map((p) => (p.as && p.as !== p.name ? `${p.name} (as ${p.as})` : p.name))
              .join(", ")}
          </div>
        ) : null}
      </div>
    </div>
  );
}

// DuplicateCard shows a scene you hold 2+ library copies of: each copy's
// resolution/size/path, the highest-res marked "best", with a two-click
// Delete (irreversible — removes the Stash scene + file). Deleting reloads
// the list, so a scene drops off once a single copy remains.
function DuplicateCard({
  dup,
  onDeleted,
}: {
  dup: DuplicateScene;
  onDeleted: () => void;
}) {
  const [busyId, setBusyId] = useState<string | null>(null);
  const [armId, setArmId] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const armTimer = useRef<number | undefined>(undefined);

  async function del(sceneId: string) {
    // First click arms; second within 4s commits.
    if (armId !== sceneId) {
      setArmId(sceneId);
      clearTimeout(armTimer.current);
      armTimer.current = window.setTimeout(() => setArmId(null), 4000);
      return;
    }
    clearTimeout(armTimer.current);
    setArmId(null);
    setBusyId(sceneId);
    setErr(null);
    try {
      await destroyScene(sceneId);
      onDeleted();
    } catch (e) {
      setErr((e as Error).message);
      setBusyId(null);
    }
  }

  return (
    <div className="dup-card">
      <div className="dup-card-head">
        {dup.image_url ? (
          <img
            className="dup-card-thumb"
            src={dup.image_url}
            alt=""
            loading="lazy"
            onLoad={(e) => {
              // Same contract as Discover: stop the placeholder once the image
              // is there. Without this every card on the page keeps an infinite
              // CSS animation repainting behind a picture that already arrived.
              (e.currentTarget as HTMLImageElement)
                .parentElement?.classList.add("thumb-loaded");
            }}
            onError={(e) => {
              const img = e.currentTarget as HTMLImageElement;
              img.style.display = "none";
              // A thumbnail that failed is not still loading.
              img.parentElement?.classList.add("thumb-failed");
            }}
          />
        ) : null}
        <div className="dup-card-headinfo">
          <div className="dup-card-title">{dup.title || dup.stashdb_id}</div>
          <div className="dup-card-sub">
            {dup.copies.length} copies
            {dup.date ? ` · ${dup.date}` : ""}
            {dup.studio ? ` · ${dup.studio}` : ""}
          </div>
        </div>
      </div>
      <div className="dup-card-copies">
        {dup.copies.map((c, i) => (
          <div className="dup-copy-row" key={c.scene_id}>
            <span
              className={"scene-res-badge inline res-" + resTierFromLabel(c.resolution)}
            >
              {c.resolution || "?"}
            </span>
            {i === 0 && <span className="dup-best">best</span>}
            {c.size ? (
              <span className="dup-copy-size">{humanSize(c.size)}</span>
            ) : null}
            {c.path ? (
              <span className="dup-copy-path" title={c.path}>
                {c.path}
              </span>
            ) : null}
            <button
              className={"dup-del" + (armId === c.scene_id ? " armed" : "")}
              disabled={busyId != null}
              onClick={() => del(c.scene_id)}
              title="Delete this copy (removes the Stash scene + file)"
            >
              {busyId === c.scene_id
                ? "Deleting…"
                : armId === c.scene_id
                  ? "Confirm delete"
                  : "Delete"}
            </button>
          </div>
        ))}
      </div>
      {err && <div className="grab-delete-err">{err}</div>}
    </div>
  );
}

