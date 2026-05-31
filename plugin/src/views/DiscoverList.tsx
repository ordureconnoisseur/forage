import { useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import {
  DiscoverPerformer,
  DiscoverResponse,
  DiscoverScene,
  fetchDiscover,
  performerImageURL,
} from "../api";
import WatchControl from "../WatchControl";

// DiscoverList shows recent StashDB scenes (default last 30 days)
// featuring ≥1 of the user's local-library performers, filtering out
// scenes they already own. Each card shows performer chips for the
// library performers in the scene; clicking a chip jumps to that
// performer's missing-scenes view.
//
// Data is cached server-side in recent_scene_cache, rebuilt every 12h
// by the daemon's refresher. The view re-queries on filter changes
// but doesn't poll aggressively — scenes don't change minute-to-minute.

const SLOW_POLL_MS = 60_000;

const DAYS_PRESETS = [7, 30, 60, 90] as const;

// Trending pulls the daemon's full top-50 — the carousel paginates
// through them 5 at a time so this fits comfortably in one row.
const TRENDING_LIMIT = 50;

export default function DiscoverList({
  onPickPerformer,
  onPickScene,
}: {
  onPickPerformer: (localID: string) => void;
  // Navigate straight to a scene's release-search page. Carries the
  // optional performer name so the placer can drop the file under
  // <library>/<performer>/ when the user grabs from this jump-point.
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [data, setData] = useState<DiscoverResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [days, setDays] = useState<number>(() => {
    const stored = parseInt(localStorage.getItem("forage.discover.days") || "", 10);
    return DAYS_PRESETS.includes(stored as (typeof DAYS_PRESETS)[number])
      ? stored
      : 30;
  });
  const [favoriteOnly, setFavoriteOnly] = useState<boolean>(
    () => localStorage.getItem("forage.discover.favorite_only") === "true",
  );
  const [q, setQ] = useState("");
  const lastFetch = useRef(0);

  // Persist filter prefs across reloads.
  useEffect(() => {
    localStorage.setItem("forage.discover.days", String(days));
  }, [days]);
  useEffect(() => {
    localStorage.setItem("forage.discover.favorite_only", String(favoriteOnly));
  }, [favoriteOnly]);

  // Fetch on mount + whenever filters change. Light polling for
  // background refresh — the underlying cache only changes every 12h
  // so this is mostly a no-op.
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;

    async function tick() {
      if (cancelled) return;
      if (document.hidden) {
        timer = window.setTimeout(tick, SLOW_POLL_MS * 2);
        return;
      }
      try {
        const r = await fetchDiscover({
          days,
          favoriteOnly,
          trendingLimit: TRENDING_LIMIT,
        });
        if (cancelled) return;
        setData(r);
        setError(null);
        lastFetch.current = Date.now();
      } catch (e) {
        if (cancelled) return;
        setError((e as Error).message);
      } finally {
        if (!cancelled) setLoading(false);
      }
      if (cancelled) return;
      timer = window.setTimeout(tick, SLOW_POLL_MS);
    }
    setLoading(true);
    tick();
    const onVis = () => {
      if (!document.hidden && Date.now() - lastFetch.current > SLOW_POLL_MS) {
        if (timer) clearTimeout(timer);
        tick();
      }
    };
    document.addEventListener("visibilitychange", onVis);
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVis);
    };
  }, [days, favoriteOnly]);

  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    if (!needle) return data.scenes;
    return data.scenes.filter((s) => {
      if ((s.title || "").toLowerCase().includes(needle)) return true;
      if ((s.studio_name || "").toLowerCase().includes(needle)) return true;
      return s.performers.some((p) => p.name.toLowerCase().includes(needle));
    });
  }, [data, q]);

  if (loading && !data)
    return <div className="empty">Loading discover…</div>;
  if (error)
    return <div className="empty error">Failed to load: {error}</div>;
  if (!data) return null;

  return (
    <div>
      <div className="page-header">
        <h2>Discover</h2>
        <div className="meta">
          Last {data.days} days · {data.scenes.length} new scene
          {data.scenes.length === 1 ? "" : "s"} from your performers
          {data.refreshed_at > 0 && (
            <> · cache refreshed {relativeTime(data.refreshed_at)}</>
          )}
        </div>
      </div>

      {data.trending && data.trending.length > 0 && (
        <section className="trending-section">
          <div className="trending-head">
            <h3 className="section-header">
              Trending on StashDB · {data.trending.length}
            </h3>
            {data.trending_refreshed_at > 0 && (
              <div className="trending-meta">
                <span className="muted">
                  refreshed {relativeTime(data.trending_refreshed_at)}
                </span>
              </div>
            )}
          </div>
          <TrendingCarousel
            scenes={data.trending}
            onPickPerformer={onPickPerformer}
            onPickScene={onPickScene}
          />
        </section>
      )}

      <h3 className="section-header from-your-performers">
        From your performers
      </h3>

      <div className="controls">
        <input
          type="text"
          placeholder="Filter by title, studio, performer…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <select
          className="discover-days"
          value={days}
          onChange={(e) => setDays(parseInt(e.target.value, 10))}
        >
          {DAYS_PRESETS.map((d) => (
            <option key={d} value={d}>
              Last {d} days
            </option>
          ))}
        </select>
        <label className="check">
          <input
            type="checkbox"
            checked={favoriteOnly}
            onChange={(e) => setFavoriteOnly(e.target.checked)}
          />
          Favourites only
        </label>
        <span className="count">
          {filtered.length} / {data.scenes.length}
        </span>
      </div>

      {filtered.length === 0 ? (
        <div className="empty">
          {data.scenes.length === 0
            ? "No recent scenes found from your performers. The cache refreshes every 12 hours."
            : "No scenes match this filter."}
        </div>
      ) : (
        <div className="scene-grid">
          {filtered.map((s) => (
            <DiscoverCard
              key={s.stashdb_id}
              s={s}
              onPickPerformer={onPickPerformer}
              onPickScene={onPickScene}
            />
          ))}
        </div>
      )}
    </div>
  );
}

// TrendingCarousel renders the trending list in fixed pages of 5 cards
// with chevron pagination on either side. No transform/translate math —
// each page just slices the scenes array. Cards inside a page share
// the row evenly so 5 always fill the strip; the final (partial) page
// is centred when it has fewer than 5.
const TRENDING_PAGE_SIZE = 5;

function TrendingCarousel({
  scenes,
  onPickPerformer,
  onPickScene,
}: {
  scenes: DiscoverScene[];
  onPickPerformer: (localID: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [page, setPage] = useState(0);
  const pageCount = Math.max(1, Math.ceil(scenes.length / TRENDING_PAGE_SIZE));
  // Clamp if the slider just reduced trendingLimit and the current
  // page is past the new last page.
  const safePage = Math.min(page, pageCount - 1);
  const pageScenes = scenes.slice(
    safePage * TRENDING_PAGE_SIZE,
    (safePage + 1) * TRENDING_PAGE_SIZE,
  );
  const canLeft = safePage > 0;
  const canRight = safePage < pageCount - 1;

  return (
    <div className="trending-carousel">
      <button
        className="carousel-chev left"
        onClick={() => setPage((p) => Math.max(0, p - 1))}
        disabled={!canLeft}
        aria-label="Previous"
      >
        ‹
      </button>
      <div className="carousel-row">
        {pageScenes.map((s) => (
          <TrendingCard
            key={s.stashdb_id}
            s={s}
            onPickPerformer={onPickPerformer}
            onPickScene={onPickScene}
          />
        ))}
      </div>
      <button
        className="carousel-chev right"
        onClick={() => setPage((p) => Math.min(pageCount - 1, p + 1))}
        disabled={!canRight}
        aria-label="Next"
      >
        ›
      </button>
    </div>
  );
}

// TrendingCard is the compact variant — image + title + single meta
// line. Thumb click is the primary forage action: navigate in-app to
// the scene's release-search page. Small overlay link goes to StashDB
// for verification.
function TrendingCard({
  s,
  onPickPerformer,
  onPickScene,
}: {
  s: DiscoverScene;
  onPickPerformer: (localID: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  // First library performer (if any) gets a tiny chip beneath the
  // title — keeps the card height bounded while still surfacing the
  // "from your library" signal.
  const primaryLibraryPerformer = s.performers[0];
  return (
    <div className="trending-card">
      <div className="scene-thumb-wrap">
        <button
          type="button"
          className="scene-thumb scene-thumb-button"
          onClick={() => onPickScene(s.stashdb_id, primaryLibraryPerformer?.name)}
          title={s.title ? `Find releases for "${s.title}"` : "Find releases"}
        >
          {s.image_url ? (
            <img
              src={s.image_url}
              alt=""
              loading="lazy"
              onError={(e) => {
                (e.currentTarget as HTMLImageElement).style.display = "none";
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
            ↗
          </a>
        </button>
        <WatchControl
          scene={{
            stashdb_id: s.stashdb_id,
            title: s.title,
            date: s.release_date,
            studio: s.studio_name,
            image_url: s.image_url,
          }}
          performerName={primaryLibraryPerformer?.name}
          performerId={primaryLibraryPerformer?.stash_id}
          initialStatus={s.watch_status || ""}
          variant="overlay"
        />
      </div>
      <div className="trending-card-body">
        <div className="trending-card-title" title={s.title || ""}>
          {s.title || "(untitled)"}
        </div>
        <div className="trending-card-meta">
          {s.release_date && <span>{s.release_date}</span>}
          {s.release_date && s.studio_name && <span> · </span>}
          {s.studio_name && <span>{s.studio_name}</span>}
        </div>
        {primaryLibraryPerformer && (
          <PerfChip
            p={primaryLibraryPerformer}
            onPick={() => onPickPerformer(primaryLibraryPerformer.stash_id)}
            extraLabel={
              s.performers.length > 1
                ? ` +${s.performers.length - 1}`
                : undefined
            }
          />
        )}
      </div>
    </div>
  );
}

function DiscoverCard({
  s,
  onPickPerformer,
  onPickScene,
}: {
  s: DiscoverScene;
  onPickPerformer: (localID: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  // Pick a library performer to pass to the placer when the user
  // grabs a release of this scene — whichever performer is in their
  // library determines the destination folder. If multiple library
  // performers feature, take the first one (sufficient for one folder
  // per grab; the scene file ends up tagged with all performers in
  // Stash regardless of folder choice).
  const primaryLibraryPerformer = s.performers[0];
  return (
    <div className="scene-card discover-card">
      <div className="scene-thumb-wrap">
        <button
          type="button"
          className="scene-thumb scene-thumb-button"
          onClick={() => onPickScene(s.stashdb_id, primaryLibraryPerformer?.name)}
          title={s.title ? `Find releases for "${s.title}"` : "Find releases"}
        >
          {s.image_url ? (
            <img
              src={s.image_url}
              alt=""
              loading="lazy"
              onError={(e) => {
                (e.currentTarget as HTMLImageElement).style.display = "none";
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
            ↗
          </a>
        </button>
        <WatchControl
          scene={{
            stashdb_id: s.stashdb_id,
            title: s.title,
            date: s.release_date,
            studio: s.studio_name,
            image_url: s.image_url,
          }}
          performerName={primaryLibraryPerformer?.name}
          performerId={primaryLibraryPerformer?.stash_id}
          initialStatus={s.watch_status || ""}
          variant="overlay"
        />
      </div>
      <div className="scene-info">
        <div className="title">{s.title || "(untitled)"}</div>
        <div className="meta">
          {s.release_date && <span>{s.release_date}</span>}
          {s.release_date && s.studio_name && <span> · </span>}
          {s.studio_name && <span>{s.studio_name}</span>}
        </div>
        {s.performers.length > 0 && (
          <div className="perf-chips">
            {s.performers.map((p) => (
              <PerfChip
                key={p.stash_id}
                p={p}
                onPick={() => onPickPerformer(p.stash_id)}
              />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// PerfChip is the clickable performer pill used inside scene cards.
// Click navigates to the performer's missing-scenes view; hovering
// opens a fixed-position hovercard with image + stats. The hovercard
// itself is non-interactive — moving the cursor onto it hides it,
// which is fine because all the actions are on the chip already.
const HOVER_OPEN_DELAY_MS = 250;

function PerfChip({
  p,
  onPick,
  extraLabel,
}: {
  p: DiscoverPerformer;
  onPick: () => void;
  // Optional suffix (e.g. " +2") for compact-chip use cases where
  // multiple performers collapse into a single chip.
  extraLabel?: string;
}) {
  const [anchor, setAnchor] = useState<DOMRect | null>(null);
  const openTimer = useRef<number | undefined>(undefined);

  const cancelOpen = () => {
    if (openTimer.current) {
      window.clearTimeout(openTimer.current);
      openTimer.current = undefined;
    }
  };

  const onEnter = (e: React.MouseEvent<HTMLButtonElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    cancelOpen();
    openTimer.current = window.setTimeout(() => {
      setAnchor(rect);
    }, HOVER_OPEN_DELAY_MS);
  };
  const onLeave = () => {
    cancelOpen();
    setAnchor(null);
  };

  useEffect(() => () => cancelOpen(), []);

  return (
    <>
      <button
        className={"perf-chip" + (p.favorite ? " fav" : "")}
        onClick={onPick}
        onMouseEnter={onEnter}
        onMouseLeave={onLeave}
        onFocus={(e) => setAnchor(e.currentTarget.getBoundingClientRect())}
        onBlur={() => setAnchor(null)}
        title={`Open ${p.name}'s missing scenes`}
      >
        {p.name}
        {p.favorite ? " ♥" : ""}
        {extraLabel || ""}
      </button>
      {anchor && <PerformerHovercard p={p} anchor={anchor} />}
    </>
  );
}

function PerformerHovercard({
  p,
  anchor,
}: {
  p: DiscoverPerformer;
  anchor: DOMRect;
}) {
  // Position the card directly below the chip, anchored to the chip's
  // left edge by default. Flip above when there's no room below; clamp
  // to the viewport horizontally so cards near the right edge don't
  // overflow. Fixed positioning avoids issues with overflow:hidden
  // ancestors (e.g. the carousel viewport).
  const CARD_W = 240;
  const GAP = 8;
  const vw = window.innerWidth;
  const vh = window.innerHeight;

  let left = anchor.left;
  if (left + CARD_W > vw - GAP) left = vw - CARD_W - GAP;
  if (left < GAP) left = GAP;

  const showBelow = anchor.bottom + 280 < vh;
  const top = showBelow ? anchor.bottom + GAP : anchor.top - GAP;
  const transform = showBelow ? "" : "translateY(-100%)";

  const imgURL = performerImageURL(p.stash_id);
  const missing = Math.max(
    0,
    (p.total_stashdb_scenes || 0) - (p.owned_scenes_count || 0),
  );
  const lastRelease =
    p.last_release_unix && p.last_release_unix > 0
      ? new Date(p.last_release_unix * 1000).toISOString().slice(0, 10)
      : null;

  const card = (
    <div
      className="perf-hovercard"
      style={{ left, top, transform, width: CARD_W }}
    >
      <div className="perf-hovercard-img">
        {imgURL ? (
          <img
            src={imgURL}
            alt=""
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.display = "none";
            }}
          />
        ) : null}
      </div>
      <div className="perf-hovercard-body">
        <div className="perf-hovercard-name">
          {p.name}
          {p.favorite && (
            <span className="perf-hovercard-fav" aria-label="Favourite">
              ♥
            </span>
          )}
        </div>
        <dl className="perf-hovercard-stats">
          {p.scene_count != null && (
            <>
              <dt>In library</dt>
              <dd>{p.scene_count} scenes</dd>
            </>
          )}
          {p.total_stashdb_scenes != null && p.total_stashdb_scenes > 0 && (
            <>
              <dt>On StashDB</dt>
              <dd>{p.total_stashdb_scenes} total</dd>
            </>
          )}
          {p.total_stashdb_scenes != null && p.total_stashdb_scenes > 0 && (
            <>
              <dt>Missing</dt>
              <dd>{missing}</dd>
            </>
          )}
          {lastRelease && (
            <>
              <dt>Last release</dt>
              <dd>{lastRelease}</dd>
            </>
          )}
        </dl>
      </div>
    </div>
  );
  return createPortal(card, document.body);
}

function relativeTime(unix: number): string {
  if (!unix) return "";
  const ageSec = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (ageSec < 60) return `${ageSec}s ago`;
  if (ageSec < 3600) return `${Math.floor(ageSec / 60)}m ago`;
  if (ageSec < 86_400) return `${Math.floor(ageSec / 3600)}h ago`;
  return `${Math.floor(ageSec / 86_400)}d ago`;
}
