import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import {
  addWatches,
  DiscoverPerformer,
  DiscoverResponse,
  DiscoverScene,
  addPerformerFromStashDB,
  fetchDiscover,
  fetchDiscoverBoxes,
  type DiscoverBox,
  performerImageURL,
  type AddWatchReq,
  stashdbThumbURL,
} from "../api";
import { peek, store } from "../swr";
import WatchControl from "../WatchControl";
import { filterGlyph } from "../format";
import { CheckIcon, ChevronIcon, ExternalIcon, GenderIcon, HeartIcon, PlusIcon } from "../icons";
import PerformerPicks from "./PerformerPicks";

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

// RENDER_CAP bounds what is MOUNTED, not what is fetched or how far back the
// window reaches — the days control above is the setting for that, and this
// must never look like a second one. A 90-day window returns hundreds of
// scenes, and mounting ~390 cards means ~390 lazy image requests queued
// against one host plus the DOM to match. Showing the first 300 keeps the page
// responsive; the count below the grid says when anything is held back, so the
// answer is "narrow the window" rather than a silent truncation.
const RENDER_CAP = 300;

// Trending pulls the daemon's full top-50 — the carousel paginates
// through them 5 at a time so this fits comfortably in one row.
const TRENDING_LIMIT = 50;

// Cache key for one Discover request. Must name every input that changes
// the response, or switching day window would show the wrong cached set.
function discoverKey(
  days: number,
  favoriteOnly: boolean,
  filter: string,
  box: string,
): string {
  return `/discover?days=${days}&fav=${favoriteOnly}&filter=${filter}&box=${box}`;
}

export default function DiscoverList({
  onPickPerformer,
  onPickScene,
  onSeeTrending,
}: {
  onPickPerformer: (localID: string) => void;
  // Navigate straight to a scene's release-search page. Carries the
  // optional performer name so the placer can drop the file under
  // <library>/<performer>/ when the user grabs from this jump-point.
  onPickScene: (stashDBID: string, performerName?: string) => void;
  // Opens the full trending ranking for the source being browsed.
  onSeeTrending: (box?: string) => void;
}) {
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
  // Deployment-configured content filter ("" = all). Persisted like the
  // other Discover view prefs.
  const [contentFilter, setContentFilter] = useState<string>(
    () => localStorage.getItem("forage.discover.filter") || "",
  );
  // Declared after the three inputs that key the request, so the initial
  // state can be seeded from the cache for THIS key: navigating back paints
  // the previous view immediately instead of spinning while the poll below
  // refetches. Same treatment as WatchingList, and for the same reason —
  // this view owns a poll timer, so useCached would be a second scheduler.
  // Which stash-box the feed reads. "" = StashDB, the cached feed that backs
  // the rest of forage. Anything else is a live browse of a secondary box
  // (see the daemon's discover_box.go). Remembered, because picking a source
  // is a mood rather than a per-visit decision.
  const [box, setBox] = useState<string>(
    () => localStorage.getItem("forage.discover.box") || "",
  );
  const [boxes, setBoxes] = useState<DiscoverBox[]>([]);
  useEffect(() => {
    localStorage.setItem("forage.discover.box", box);
  }, [box]);
  useEffect(() => {
    let cancelled = false;
    fetchDiscoverBoxes()
      .then((r) => {
        if (cancelled) return;
        setBoxes(r.boxes || []);
        // A remembered box that has gone away or stopped answering must not
        // strand the page on a source it cannot load.
        if (
          box &&
          !(r.boxes || []).some(
            (b) => b.endpoint === box && !b.primary && !b.unreachable,
          )
        ) {
          setBox("");
        }
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
    // Once per mount: the list comes from Stash's settings and the daemon
    // caches it for minutes anyway.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const [data, setData] = useState<DiscoverResponse | null>(() =>
    peek<DiscoverResponse>(discoverKey(days, favoriteOnly, contentFilter, box)),
  );
  const [loading, setLoading] = useState(() => data === null);
  const [error, setError] = useState<string | null>(null);
  const lastFetch = useRef(0);
  // Multi-select over the "From your performers" grid, same interaction
  // as MissingScenes: Select flips cards into toggle mode, the bottom
  // bar bulk-watches the chosen scenes. Watch is the one action that
  // makes sense across a multi-performer set (grab/collection flows are
  // per-performer by design). Trending stays out of selection — its
  // compact carousel cards are navigation-first.
  const [selecting, setSelecting] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [watchBusy, setWatchBusy] = useState(false);
  const [watchedMsg, setWatchedMsg] = useState<string | null>(null);
  // Bumped after a bulk watch so the refetch picks up the new
  // watch_status badges without waiting for the next poll.
  const [reloadKey, setReloadKey] = useState(0);

  // Persist filter prefs across reloads.
  useEffect(() => {
    localStorage.setItem("forage.discover.days", String(days));
  }, [days]);
  useEffect(() => {
    localStorage.setItem("forage.discover.favorite_only", String(favoriteOnly));
  }, [favoriteOnly]);
  useEffect(() => {
    localStorage.setItem("forage.discover.filter", contentFilter);
  }, [contentFilter]);

  // Fetch on mount + whenever filters change. Light polling for
  // background refresh — the underlying cache only changes every 12h
  // so this is mostly a no-op.
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    let inFlight = false;

    async function tick() {
      // inFlight: while a fetch is awaited, `timer` holds an already-fired
      // timeout id, so the visibilitychange handler's clearTimeout is a
      // no-op and its tick() would fork a second self-rescheduling loop,
      // permanently doubling the poll rate (same guard as GrabsList).
      if (cancelled || inFlight) return;
      if (document.hidden) {
        timer = window.setTimeout(tick, SLOW_POLL_MS * 2);
        return;
      }
      inFlight = true;
      try {
        const r = await fetchDiscover({
          box: box || undefined,
          filter: contentFilter || undefined,
          days,
          favoriteOnly,
          trendingLimit: TRENDING_LIMIT,
        });
        if (cancelled) return;
        store(discoverKey(days, favoriteOnly, contentFilter, box), r);
        setData(r);
        setError(null);
        lastFetch.current = Date.now();
      } catch (e) {
        if (cancelled) return;
        setError((e as Error).message);
      } finally {
        inFlight = false;
        if (!cancelled) setLoading(false);
      }
      if (cancelled) return;
      timer = window.setTimeout(tick, SLOW_POLL_MS);
    }
    // Switching day window / filter is a different key: show that key's
    // cached answer if we have one rather than blanking to a spinner.
    const hit = peek<DiscoverResponse>(discoverKey(days, favoriteOnly, contentFilter, box));
    if (hit) {
      setData(hit);
      setLoading(false);
    } else {
      setLoading(true);
    }
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
  }, [days, favoriteOnly, contentFilter, box, reloadKey]);

  // Changing the day window / favourites filter swaps the scene set out
  // from under a selection — drop it rather than keep invisible picks.
  useEffect(() => {
    setSelecting(false);
    setSelected(new Set());
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

  // Select-all works on the FILTERED grid (what the user can see); the
  // bulk action resolves ids against the full scene list, so picks made
  // before a narrowing filter still apply.
  const visibleIds = filtered.map((s) => s.stashdb_id);
  // Whether the render cap is actually biting, which is the only thing
  // that distinguishes the readout from a plain total.
  const capped = filtered.length > RENDER_CAP;
  const allSelected =
    visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));

  // Watch every selected scene as ONE batch. Each scene carries its OWN primary
  // library performer (unlike the per-performer pages) so each watch lands in
  // the right folder later; the batch groups them in the Watching tab as a
  // "Discover · N scenes" run. No quality target (best release by preference).
  const watchSelected = async () => {
    setWatchBusy(true);
    const chosen = data.scenes.filter((s) => selected.has(s.stashdb_id));
    const watches: AddWatchReq[] = chosen.map((s) => ({
      stashdb_id: s.stashdb_id,
      source: box || undefined,
      title: s.title || "",
      date: s.release_date,
      studio: s.studio_name,
      image_url: s.image_url,
      performer_name: s.performers[0]?.name,
      performer_id: s.performers[0]?.stash_id,
      performers: s.performers.map((p) => p.name),
    }));
    try {
      await addWatches({
        batch_label: `Discover · ${chosen.length} scene${chosen.length === 1 ? "" : "s"}`,
        watches,
      });
    } catch {
      // best-effort; the toast still confirms intent
    }
    setWatchBusy(false);
    const n = chosen.length;
    exitSelect();
    setWatchedMsg(`Watching ${n} scene${n === 1 ? "" : "s"}`);
    window.setTimeout(() => setWatchedMsg(null), 3500);
    setReloadKey((k) => k + 1);
  };

  // What to call the active source in the headings. On a secondary box the
  // StashDB wording is not merely imprecise, it is false: there is no day
  // window, the scenes are not "from your performers" (that would need
  // performers identified on THAT box), and the carousel is not StashDB's.
  const activeBox = boxes.find((b) => (b.primary ? "" : b.endpoint) === box);
  const boxName = activeBox?.name || (box ? "this stash-box" : "StashDB");

  return (
    <div>
      <div className="page-header">
        <h2>Discover</h2>
        <div className="meta">
          {box ? (
            <>
              {data.scenes.length} recent scene
              {data.scenes.length === 1 ? "" : "s"} on {boxName} · live, not
              cached
            </>
          ) : (
            <>
              {/* "39 of 380" whenever a filter is narrowing the window. The
                  sentence describes the whole window while the number is the
                  filtered count, so with Favourites ticked it read as forage
                  having lost 341 scenes rather than as a filter doing its
                  job. Same grammar as the render cap's "300/380". */}
              Last {data.days} days ·{" "}
              {data.scene_total > data.scenes.length ? (
                <>
                  <strong>{data.scenes.length}</strong> of {data.scene_total}{" "}
                  scenes from your performers
                </>
              ) : (
                <>
                  {data.scenes.length} new scene
                  {data.scenes.length === 1 ? "" : "s"} from your performers
                </>
              )}
              {data.refreshed_at > 0 && (
                <> · cache refreshed {relativeTime(data.refreshed_at)}</>
              )}
            </>
          )}
        </div>
      </div>

      {data.trending && data.trending.length > 0 && (
        <section className="trending-section">
          <div className="trending-head">
            {/* The title may truncate; the numbers may not. On a narrow
                screen the source name drops out entirely — the page header
                one row up already says which box this is — and "refreshed"
                drops to just the age, because position carries the word. */}
            <h3 className="section-header">
              Trending<span className="trending-src"> on {boxName}</span>
            </h3>
            <span className="trending-count muted">{data.trending.length}</span>
            {data.trending_refreshed_at > 0 && (
              <div className="trending-meta muted">
                <span className="trending-refreshed-word">refreshed </span>
                {relativeTime(data.trending_refreshed_at)}
              </div>
            )}
            {/* The carousel is the top of a ranking that keeps going. This is
                the way further down it, and it earns its place in the header
                because the carousel's own chevrons stop at the cached 50 with
                nothing saying why. */}
            <button
              type="button"
              className="trending-all"
              onClick={() => onSeeTrending(box || undefined)}
              title={`Browse the full ${boxName} trending ranking`}
            >
              See all
              <ChevronIcon />
            </button>
          </div>
          <TrendingCarousel
            scenes={data.trending}
            box={box}
            onPickPerformer={onPickPerformer}
            onPickScene={onPickScene}
          />
        </section>
      )}

      {/* Between the two: trending SCENES above, your own performers' new
          releases below, and the people worth adding in between. All three
          are "what should I look at", ordered by how far they sit from the
          library you already have. */}
      <PerformerPicks />

      <h3 className="section-header from-your-performers">
        {box ? `Recent on ${boxName}` : "From your performers"}
      </h3>

      {/* Sorted by JOB, not by control type. The old bar held seven things in
          one row because nothing had decided what kind of thing each was.
          They are four: a query, a scope, a mode, and a readout — and only
          the scope is a filter. */}
      <div className="filter-bar">
        <input
          className="filter-q"
          type="text"
          placeholder="Filter by title, studio, performer…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        {/* Scope: four ways of narrowing, one idiom. Two open a menu, two
            toggle; a select and a toggle differ only by the caret. Swipes on
            a phone, wraps on a desktop, and simply gets shorter when a
            secondary box hides half of it. */}
        <div className="filter-scope">
          {/* One control, not a chip per box. A native select collapses to the
              active source and opens on press, so adding a fifth stash-box in
              Stash costs no width here. Unreachable boxes stay in the list but
              un-pickable, with the reason in their label. */}
          {boxes.filter((b) => !b.unreachable).length > 1 && (
            <select
              aria-label="Source"
              value={box}
              title={
                box
                  ? `Browsing ${boxName}. Live results; owned and missing tracking stays on StashDB.`
                  : "StashDB, your library's source, with owned and missing tracking"
              }
              onChange={(e) => setBox(e.target.value)}
            >
              {boxes.map((b) => (
                <option
                  key={b.endpoint}
                  value={b.primary ? "" : b.endpoint}
                  disabled={!!b.unreachable}
                >
                  {b.name}
                  {b.unreachable ? ` (${b.unreachable})` : ""}
                </option>
              ))}
            </select>
          )}
          {/* Both of these read the StashDB cache: the day window bounds
              recent_scene_cache, and "favourites" means a local performer.
              Neither has meaning against a live secondary box, so rather than
              leave two dead controls they step aside. */}
          {!box && (
            <select
              aria-label="Window"
              value={days}
              onChange={(e) => setDays(parseInt(e.target.value, 10))}
            >
              {DAYS_PRESETS.map((d) => (
                <option key={d} value={d}>
                  {d} days
                </option>
              ))}
            </select>
          )}
          {!box && (
            <button
              type="button"
              className="scope-pill"
              aria-pressed={favoriteOnly}
              onClick={() => setFavoriteOnly(!favoriteOnly)}
            >
              <span className="scope-glyph" aria-hidden="true">
                <HeartIcon filled={favoriteOnly} />
              </span>
              Favourites
            </button>
          )}
          {Object.keys(data.filters ?? {})
            .sort()
            .map((f) => {
              const glyph = filterGlyph(data.filters?.[f]);
              const on = contentFilter === f;
              return (
                <button
                  key={f}
                  type="button"
                  className="scope-pill"
                  aria-pressed={on}
                  title={`Only show ${f} content (deployment filter)`}
                  onClick={() => setContentFilter(on ? "" : f)}
                >
                  {/* The glyph keeps its glyph AND gains its name: the
                      filter's config key is the word the tooltip was
                      spelling out, so a deployment adding a fifth filter
                      needs no design decision, and touch — which has no
                      hover — stops depending on a title attribute. */}
                  {glyph && (
                    <span className="scope-glyph" aria-hidden="true">
                      <GenderIcon kind={glyph} />
                    </span>
                  )}
                  {f}
                </button>
              );
            })}
        </div>
      </div>

      {/* Readout and mode. Neither is a filter, so neither lives in the bar:
          the count is text, and Select acts on the grid rather than narrowing
          it. */}
      <div className="list-head">
        {/* The fraction IS the warning. "300/368" says not-all-of-it in six
            characters, which is what a full-width amber banner was spending
            44px and a two-line sentence to say. The cap stays legible as a
            cap because the numerator is amber — colour on one numeral rather
            than around a whole box. */}
        <span
          className={"list-readout" + (capped ? " is-capped" : "")}
          title={
            capped
              ? `Showing the first ${RENDER_CAP} of ${filtered.length}. Narrow the window or the filter to see the rest.`
              : undefined
          }
        >
          {capped ? (
            <>
              <span className="shown">{RENDER_CAP}</span>
              <span className="sep">/</span>
              {filtered.length}
            </>
          ) : filtered.length === data.scenes.length ? (
            filtered.length
          ) : (
            <>
              {filtered.length}
              <span className="sep">/</span>
              {data.scenes.length}
            </>
          )}
        </span>
        {filtered.length > 0 && (
          <div className="list-head-actions">
            {selecting ? (
              <>
                <button
                  onClick={() =>
                    setSelected((prev) => {
                      const next = new Set(prev);
                      if (allSelected)
                        visibleIds.forEach((id) => next.delete(id));
                      else visibleIds.forEach((id) => next.add(id));
                      return next;
                    })
                  }
                >
                  {allSelected ? "Clear all" : "Select all"}
                </button>
                <button onClick={exitSelect}>Cancel</button>
              </>
            ) : (
              <button onClick={() => setSelecting(true)}>Select</button>
            )}
          </div>
        )}
      </div>

      {filtered.length === 0 ? (
        <div className="empty">
          {data.scenes.length === 0
            ? box
              ? `No scenes came back from ${boxName}.`
              : "No recent scenes found from your performers. The cache refreshes every 12 hours."
            : "No scenes match this filter."}
        </div>
      ) : (
        <div className="scene-grid">
          {filtered.slice(0, RENDER_CAP).map((s) => (
            <DiscoverCard
              box={box}
              key={s.stashdb_id}
              s={s}
              selecting={selecting}
              selected={selected.has(s.stashdb_id)}
              onToggle={() => toggleSelected(s.stashdb_id)}
              onPickPerformer={onPickPerformer}
              onPickScene={onPickScene}
            />
          ))}
        </div>
      )}
      {selecting && (
        <div className="ms-select-bar">
          <span className="ms-select-count">{selected.size} selected</span>
          <button
            className="ms-select-watch"
            disabled={selected.size === 0 || watchBusy}
            onClick={watchSelected}
            title="Watch all selected scenes for releases (one batch)"
          >
            {watchBusy ? "Watching…" : `Watch ${selected.size} selected`}
          </button>
        </div>
      )}
      {watchedMsg && <div className="ms-toast">{watchedMsg}</div>}
    </div>
  );
}

// TrendingCarousel renders the trending list in pages of cards with
// chevron pagination on either side. No transform/translate math —
// each page just slices the scenes array. Cards inside a page share
// the row evenly; the final (partial) page is centred. The page size
// is responsive: five 16:9 cards across a phone are unusable slivers.
function useTrendingPageSize(): number {
  const calc = () =>
    window.matchMedia("(max-width: 600px)").matches
      ? 2
      : window.matchMedia("(max-width: 960px)").matches
        ? 3
        : 5;
  const [size, setSize] = useState(calc);
  useEffect(() => {
    const mqs = [
      window.matchMedia("(max-width: 600px)"),
      window.matchMedia("(max-width: 960px)"),
    ];
    const update = () => setSize(calc());
    mqs.forEach((m) => m.addEventListener("change", update));
    return () => mqs.forEach((m) => m.removeEventListener("change", update));
  }, []);
  return size;
}

function TrendingCarousel({
  scenes,
  box,
  onPickPerformer,
  onPickScene,
}: {
  scenes: DiscoverScene[];
  box: string;
  onPickPerformer: (localID: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  const [page, setPage] = useState(0);
  const pageSize = useTrendingPageSize();

  // Phones get a native swipe row instead of paged chevrons: two paged
  // cards left a column of dead chevron space and slivers of card. The
  // peeking partial card is the scroll affordance.
  if (pageSize === 2) {
    return (
      <div className="trending-carousel scroll">
        <div className="carousel-row">
          {scenes.map((s) => (
            <TrendingCard
              box={box}
              key={s.stashdb_id}
              s={s}
              onPickPerformer={onPickPerformer}
              onPickScene={onPickScene}
            />
          ))}
        </div>
      </div>
    );
  }

  const pageCount = Math.max(1, Math.ceil(scenes.length / pageSize));
  // Clamp if the slider just reduced trendingLimit (or a rotation just
  // changed the page size) and the current page is past the new last.
  const safePage = Math.min(page, pageCount - 1);
  const pageScenes = scenes.slice(
    safePage * pageSize,
    (safePage + 1) * pageSize,
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
        <ChevronIcon size={15} />
      </button>
      <div className="carousel-row">
        {pageScenes.map((s) => (
          <TrendingCard
            box={box}
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
        <ChevronIcon size={15} />
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
  box,
  onPickPerformer,
  onPickScene,
}: {
  s: DiscoverScene;
  // Stash-box this card came from ("" = StashDB), recorded on any watch.
  box: string;
  onPickPerformer: (localID: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
}) {
  // Two different performers, conflated until secondary boxes arrived. On the
  // StashDB feed the first name IS a library performer, because the feed is
  // built from the performers you follow. On a live box it usually is not, so
  // the chip navigated to an empty performer id and the card claimed someone
  // you do not have as if you did.
  //
  // namedPerformer is who the scene is billed under (seeds a release search).
  // libraryPerformer is the first one you actually own, and only that one can
  // be navigated to.
  const namedPerformer = s.performers[0];
  const libraryPerformer = s.performers.find((p) => p.local !== false && p.stash_id);
  return (
    <div className="trending-card">
      <div className="scene-thumb-wrap">
        <button
          type="button"
          className="scene-thumb scene-thumb-button"
          onClick={() => onPickScene(s.stashdb_id, namedPerformer?.name)}
          title={s.title ? `Find releases for "${s.title}"` : "Find releases"}
        >
          {s.image_url ? (
            <img
              src={stashdbThumbURL(s.image_url)}
              alt=""
              loading="lazy"
              onLoad={(e) => {
                // Stop the placeholder animating once the image is there.
                // The image covers it either way, but ~390 perpetual
                // animations repaint for nothing.
                (e.currentTarget as HTMLImageElement)
                  .parentElement?.classList.add("thumb-loaded");
              }}
              onError={(e) => {
                const img = e.currentTarget as HTMLImageElement;
                img.style.display = "none";
                // Stop the loading shimmer too, or a thumbnail that genuinely
                // failed animates forever and reads as still-arriving.
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
        </button>
        <WatchControl
          scene={{
            stashdb_id: s.stashdb_id,
            source: box || undefined,
            title: s.title,
            date: s.release_date,
            studio: s.studio_name,
            image_url: s.image_url,
          }}
          performerName={namedPerformer?.name}
          performerId={libraryPerformer?.stash_id}
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
        {libraryPerformer ? (
          <PerfChip
            p={libraryPerformer}
            onPick={() => onPickPerformer(libraryPerformer.stash_id)}
            extraLabel={
              s.performers.length > 1
                ? ` +${s.performers.length - 1}`
                : undefined
            }
          />
        ) : namedPerformer?.stashdb_id ? (
          // Nobody on this card is yours. Offer to add the billed performer
          // rather than showing a chip that navigates nowhere.
          <MissingPerfChip p={namedPerformer} box={box} />
        ) : null}
      </div>
    </div>
  );
}

// Exported for the Trending page, which renders the same card against the
// same wire shape. Two views showing scenes from the same source should not
// drift into two subtly different cards.
export function DiscoverCard({
  s,
  box,
  selecting,
  selected,
  onToggle,
  onPickPerformer,
  onPickScene,
}: {
  s: DiscoverScene;
  // Stash-box this card came from ("" = StashDB), recorded on any watch.
  box: string;
  selecting: boolean;
  selected: boolean;
  onToggle: () => void;
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
    <div
      className={
        "scene-card discover-card" +
        (selecting ? " selectable" : "") +
        (selected ? " selected" : "")
      }
      // In select mode the WHOLE card toggles, captured before the inner
      // interactive elements (thumb navigation, StashDB link, performer
      // chips, the watch overlay) can fire — same one-gesture toggle the
      // missing-scenes grid has.
      onClickCapture={
        selecting
          ? (e) => {
              e.preventDefault();
              e.stopPropagation();
              onToggle();
            }
          : undefined
      }
      role={selecting ? "button" : undefined}
      aria-pressed={selecting ? selected : undefined}
    >
      {selecting && (
        <span className="scene-check" aria-hidden="true">
          {selected ? <CheckIcon size={11} /> : null}
        </span>
      )}
      <div className="scene-thumb-wrap">
        <button
          type="button"
          className="scene-thumb scene-thumb-button"
          onClick={() => onPickScene(s.stashdb_id, primaryLibraryPerformer?.name)}
          title={s.title ? `Find releases for "${s.title}"` : "Find releases"}
        >
          {s.image_url ? (
            <img
              src={stashdbThumbURL(s.image_url)}
              alt=""
              loading="lazy"
              onLoad={(e) => {
                // Stop the placeholder animating once the image is there.
                // The image covers it either way, but ~390 perpetual
                // animations repaint for nothing.
                (e.currentTarget as HTMLImageElement)
                  .parentElement?.classList.add("thumb-loaded");
              }}
              onError={(e) => {
                const img = e.currentTarget as HTMLImageElement;
                img.style.display = "none";
                // Stop the loading shimmer too, or a thumbnail that genuinely
                // failed animates forever and reads as still-arriving.
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
        </button>
        <WatchControl
          scene={{
            stashdb_id: s.stashdb_id,
            source: box || undefined,
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
        {/* One line, never two. Date leads because it is fixed width, so the
            studio is what gets clipped rather than the line wrapping and
            making this card taller than the one beside it. */}
        <div className="meta">
          {[s.release_date, s.studio_name].filter(Boolean).join(" · ")}
        </div>
        {/* Always rendered, even with nobody in it. The row is one of the
            three fixed-height slots that stop two cards in a row disagreeing
            about height, and a slot that vanishes when a scene has no
            performers is exactly the ragged-height problem this replaced —
            measured at 241px against 272px. Empty, it reserves its min-height
            and shows nothing. */}
        <ScenePerfChips s={s} box={box} onPickPerformer={onPickPerformer} />
      </div>
    </div>
  );
}

// Rendering the pill row is a fitting problem, not a counting one.
//
// The row is one line so cards in a grid row stay the same height. What fits
// on that line depends on the card's width and on how long the names happen to
// be, so any fixed cap is wrong somewhere: three pills fit at 1440 and two at
// 390, and "Cheer Lyn Ashford" is worth two of "Riley". The old cap applied
// only to performers you do NOT have, so a card with four local performers
// sliced the fourth in half, and the "+N more" readout meant to explain the
// truncation was itself last in an overflow-hidden row and clipped with it.
//
// So the pills are measured after layout. Everyone stays mounted and in flow —
// hidden ones are made invisible rather than removed, which keeps their
// offsets measurable, so the fit survives a resize without a second pass.

// A generous bound on how many get mounted at all. The cache holds
// compilations with 89 performers; measuring 89 buttons to display two is
// waste, and the count covers the rest.
const PERF_CHIP_RENDER_CAP = 14;

// Reserve for the "+N" readout before it exists to be measured.
const MORE_CHIP_ESTIMATE = 42;

// useFitCount reports how many of a flex row's pills fit on one line.
//
// It adds up widths rather than reading positions. The first version compared
// offsetLeft against the row's width, and offsetLeft is measured from the
// offsetParent, which for an unpositioned row is somewhere far up the page: a
// pill sitting at x=0 in a 274px row reported 1044, so everything "overflowed"
// and 297 of 300 cards showed a single pill. A pill's WIDTH does not depend on
// where it sits, on whether it is painted, or on what order flex puts it in,
// which also leaves the row free to reorder without the measurement chasing
// its own tail.
function useFitCount(
  ref: React.RefObject<HTMLElement | null>,
  count: number,
  enabled: boolean,
) {
  const [fit, setFit] = useState(count);
  const moreRef = useRef<HTMLElement | null>(null);

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || !enabled) {
      setFit(count);
      return;
    }
    const measure = () => {
      const avail = el.clientWidth;
      if (avail === 0) return; // not laid out yet, or off-screen
      const gap = parseFloat(getComputedStyle(el).columnGap) || 0;
      const widths = ([...el.children] as HTMLElement[])
        .filter((c) => c !== moreRef.current)
        .map((c) => c.offsetWidth);
      const fits = (reserve: number) => {
        let used = 0;
        let n = 0;
        for (const w of widths) {
          const next = used + (n ? gap : 0) + w;
          if (next > avail - reserve) break;
          used = next;
          n++;
        }
        return n;
      };
      // If everyone fits there is no count, so no space is kept for one.
      let n = fits(0);
      if (n < widths.length) {
        n = fits((moreRef.current?.offsetWidth ?? MORE_CHIP_ESTIMATE) + gap);
      }
      // At least one, even where a single long name cannot fit: a clipped
      // name still says more than an empty row.
      setFit(Math.max(1, n));
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref, count, enabled]);

  return { fit, moreRef };
}

// ScenePerfChips renders a card's performers on one line, with whatever does
// not fit collapsed behind a count that reveals the rest on click.
function ScenePerfChips({
  s,
  box,
  onPickPerformer,
}: {
  s: DiscoverScene;
  // Stash-box these performers came from ("" = StashDB), so the "+" adds
  // them from the box that knows the id.
  box: string;
  onPickPerformer: (stashID: string) => void;
}) {
  const [showAll, setShowAll] = useState(false);
  const rowRef = useRef<HTMLDivElement | null>(null);
  // Yours first: those are the ones you chose to have, so they are the ones
  // worth the space when the line runs out.
  const local = s.performers.filter((p) => p.local !== false || !p.stashdb_id);
  const missing = s.performers.filter((p) => p.local === false && p.stashdb_id);
  const all = [...local, ...missing];
  const mounted = showAll ? all : all.slice(0, PERF_CHIP_RENDER_CAP);
  const { fit, moreRef } = useFitCount(rowRef, mounted.length, !showAll);
  const hidden = showAll ? 0 : all.length - fit;

  return (
    <div
      className={"perf-chips" + (showAll ? " is-open" : "")}
      ref={rowRef}
      // The row is a list of controls, so it gets no role of its own; the
      // count below is what carries the "there are more" information.
    >
      {mounted.map((p, i) => {
        // Kept in flow and merely unpainted: removing them would destroy the
        // offsets the fit is measured from.
        const clipped = !showAll && i >= fit;
        // order pushes clipped pills past the count, so the count lands beside
        // the last name instead of marooned at the right edge with a gap. They
        // stay in flow: only their paint is dropped.
        const style = clipped
          ? { visibility: "hidden" as const, order: 2 }
          : undefined;
        return p.local === false && p.stashdb_id ? (
          <MissingPerfChip
            key={"m" + p.stashdb_id}
            p={p}
            box={box}
            hiddenFromRow={clipped}
            style={style}
          />
        ) : (
          <PerfChip
            key={"l" + (p.stash_id || p.name)}
            p={p}
            onPick={() => onPickPerformer(p.stash_id)}
            hiddenFromRow={clipped}
            style={style}
          />
        );
      })}
      {hidden > 0 && (
        <button
          type="button"
          ref={moreRef as React.RefObject<HTMLButtonElement>}
          className="perf-chip perf-chip-more"
          onClick={() => setShowAll(true)}
          title={`Show ${hidden} more performer${hidden === 1 ? "" : "s"}`}
          aria-label={`Show ${hidden} more performer${hidden === 1 ? "" : "s"}`}
        >
          {/* Shaped like the pills it counts. As borderless dim text it read
              as something left behind by accident rather than a control. */}
          +{hidden}
        </button>
      )}
      {showAll && all.length > 1 && (
        <button
          type="button"
          className="perf-chip perf-chip-more"
          onClick={() => setShowAll(false)}
          title="Collapse the performer list"
        >
          Less
        </button>
      )}
    </div>
  );
}

// MissingPerfChip is the pill for a performer who appears on a trending
// StashDB scene but is NOT in the user's Stash. It carries a "+" that
// creates them, because there is nothing else useful to do with the pill:
// there is no local performer to navigate to and no stats to hover.
//
// Once added it becomes inert rather than disappearing — the card would
// otherwise reshuffle under the cursor, and the next Discover refresh
// re-renders it as an ordinary local pill anyway.
function MissingPerfChip({
  p,
  box,
  style,
  hiddenFromRow,
}: {
  p: DiscoverPerformer;
  box: string;
  style?: React.CSSProperties;
  hiddenFromRow?: boolean;
}) {
  const [state, setState] = useState<"idle" | "adding" | "added" | "err">("idle");
  const [msg, setMsg] = useState("");

  const add = async () => {
    if (state === "adding" || state === "added") return;
    setState("adding");
    try {
      const r = await addPerformerFromStashDB(p.stashdb_id!, p.name, box);
      setState("added");
      setMsg(r.already_present ? "already in your library" : "added to your library");
    } catch (e) {
      setState("err");
      setMsg((e as Error).message);
    }
  };

  const done = state === "added";
  const busy = state === "adding";
  // The WHOLE pill is the button.
  //
  // It used to be a span holding the name plus a separate 12px "+", so the
  // only place that did anything was a glyph smaller than a fingertip, sitting
  // inside something that looked pressable and was not. One target now, the
  // size of the pill, with the icon saying what pressing it does.
  return (
    <button
      type="button"
      className={
        "perf-chip perf-chip-missing" +
        (done ? " is-added" : "") +
        (state === "err" ? " is-err" : "")
      }
      onClick={add}
      disabled={busy || done}
      style={style}
      tabIndex={hiddenFromRow ? -1 : undefined}
      aria-hidden={hiddenFromRow || undefined}
      title={
        state === "err"
          ? msg
          : done
            ? p.name + ", " + msg
            : "Add " + p.name + " to your library"
      }
      aria-label={"Add " + p.name + " to your library"}
    >
      <span className="perf-chip-add" aria-hidden="true">
        {busy ? "…" : done ? <CheckIcon /> : state === "err" ? "!" : <PlusIcon />}
      </span>
      {p.name}
    </button>
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
  style,
  hiddenFromRow,
}: {
  p: DiscoverPerformer;
  onPick: () => void;
  style?: React.CSSProperties;
  // Measured out of the visible line: still in flow so the fit stays
  // measurable, but unpainted, so it must not be tabbable or announced.
  hiddenFromRow?: boolean;
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
        style={style}
        tabIndex={hiddenFromRow ? -1 : undefined}
        aria-hidden={hiddenFromRow || undefined}
        onClick={onPick}
        onMouseEnter={onEnter}
        onMouseLeave={onLeave}
        onFocus={(e) => setAnchor(e.currentTarget.getBoundingClientRect())}
        onBlur={() => setAnchor(null)}
        title={`Open ${p.name}'s missing scenes`}
      >
        {p.name}
        {p.favorite ? (
          <span className="perf-chip-heart">
            <HeartIcon size={9} filled />
          </span>
        ) : null}
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
            onLoad={(e) => {
              // Stop the placeholder animating once the image is there.
              // The image covers it either way, but ~390 perpetual
              // animations repaint for nothing.
              (e.currentTarget as HTMLImageElement)
                .parentElement?.classList.add("thumb-loaded");
            }}
            onError={(e) => {
              const img = e.currentTarget as HTMLImageElement;
              img.style.display = "none";
              // Stop the loading shimmer too, or a thumbnail that genuinely
              // failed animates forever and reads as still-arriving.
              img.parentElement?.classList.add("thumb-failed");
            }}
          />
        ) : null}
      </div>
      <div className="perf-hovercard-body">
        <div className="perf-hovercard-name">
          {p.name}
          {p.favorite && (
            <span className="perf-hovercard-fav" aria-label="Favourite">
              <HeartIcon size={11} filled />
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
