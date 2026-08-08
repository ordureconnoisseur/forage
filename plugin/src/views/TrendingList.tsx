import { useCallback, useEffect, useRef, useState } from "react";
import {
  fetchTrending,
  type DiscoverScene,
  type TrendingPage,
} from "../api";
import { ChevronIcon } from "../icons";
import { DiscoverCard } from "./DiscoverList";

// Trending, past the end of the carousel.
//
// Discover answers "what is new from the performers I follow". Everything on
// it is filtered through the library, which is the point of it and also its
// ceiling: it can only ever surface people already known. This page is the
// other half: the source's global ranking, unfiltered by the library, so a
// performer nobody here has ever heard of can turn up. The carousel is a
// glance at the top of that list; this is the list.
//
// Pages come live from the daemon, one request each, so what loads reflects
// the ranking at the moment it was scrolled to.

// How many scenes to ASK for per request, which is not how many cards arrive:
// the daemon drops scenes the library already holds, and on a well-stocked
// library that is most of a trending page. Measured against the live ranking,
// six requested yielded one, one and three across three pages. So ask for a
// lot and expect a little.
const PER_PAGE = 48;

// How many requests one fill may chain before giving up and waiting for the
// user to scroll. Without a bound, a stretch of the ranking that is entirely
// owned would walk to the page cap in one go.
const MAX_CHAINED = 6;

// Matches the observer's rootMargin: the distance below the fold that still
// counts as "the screen is not full yet".
const LOOKAHEAD = 800;

export default function TrendingList({
  box,
  onPickPerformer,
  onPickScene,
  onBack,
}: {
  // Endpoint of the stash-box being browsed ("" = StashDB).
  box: string;
  onPickPerformer: (localID: string) => void;
  onPickScene: (stashDBID: string, performerName?: string) => void;
  onBack: () => void;
}) {
  const [scenes, setScenes] = useState<DiscoverScene[]>([]);
  const [source, setSource] = useState("StashDB");
  const [hasMore, setHasMore] = useState(true);
  const [err, setErr] = useState("");
  // A ref, not state: the sentinel's observer fires from a closure created
  // once, and a stale `loading` would let a fast scroll queue the same page
  // several times over.
  const loading = useRef(false);
  const nextPage = useRef(1);
  const [pending, setPending] = useState(true);

  const sentinel = useRef<HTMLDivElement | null>(null);

  const loadMore = useCallback(async () => {
    if (loading.current) return;
    loading.current = true;
    setPending(true);
    try {
      // Keep going until the screen is actually full. A page of 48 can yield
      // three cards, which does not move the sentinel off screen, and an
      // IntersectionObserver only fires on a CHANGE: an element that never
      // stops intersecting never reports again, so a single fetch per event
      // leaves the scroll wedged with the sentinel sitting in view.
      for (let i = 0; i < MAX_CHAINED; i++) {
        const r: TrendingPage = await fetchTrending({
          page: nextPage.current,
          perPage: PER_PAGE,
          box: box || undefined,
        });
        nextPage.current += 1;
        setSource(r.source || "StashDB");
        setScenes((prev) => {
          // The ranking shifts under us between requests, so page 3 can
          // repeat a scene from page 2. Appending blind would render
          // duplicate keys and show the same card twice on one screen.
          const seen = new Set(prev.map((s) => s.stashdb_id));
          return prev.concat(
            (r.scenes || []).filter((s) => !seen.has(s.stashdb_id)),
          );
        });
        setHasMore(!!r.has_more);
        setErr("");
        if (!r.has_more) break;
        // Let the appended cards lay out before asking whether they filled
        // the screen; the sentinel has not moved until they have.
        await new Promise((res) => requestAnimationFrame(() => res(null)));
        const top = sentinel.current?.getBoundingClientRect().top;
        if (top == null || top > window.innerHeight + LOOKAHEAD) break;
      }
    } catch (e) {
      setErr((e as Error).message);
      // Stop the sentinel retrying into a failing endpoint forever; the
      // button below offers the retry deliberately.
      setHasMore(false);
    } finally {
      loading.current = false;
      setPending(false);
    }
  }, [box]);

  // Switching source starts over.
  useEffect(() => {
    setScenes([]);
    setHasMore(true);
    setErr("");
    nextPage.current = 1;
    void loadMore();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [box]);

  useEffect(() => {
    const el = sentinel.current;
    if (!el || !hasMore) return;
    // rootMargin buys a screen of lead time, so the next page is usually in
    // hand before the last row of the current one reaches the bottom.
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) void loadMore();
      },
      { rootMargin: `${LOOKAHEAD}px 0px` },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [hasMore, loadMore]);

  return (
    <div>
      <div className="page-header">
        <button type="button" className="trending-back" onClick={onBack}>
          <ChevronIcon />
          Discover
        </button>
        <h2>Trending on {source}</h2>
        <div className="meta">
          The whole ranking, not just your performers. This is where scenes by
          people you do not follow yet turn up. Scenes already in your library
          are left out, so this is thinner than the ranking itself.
        </div>
      </div>

      <div className="scene-grid">
        {scenes.map((s) => (
          <DiscoverCard
            key={s.stashdb_id}
            s={s}
            box={box}
            selecting={false}
            selected={false}
            onToggle={() => {}}
            onPickPerformer={onPickPerformer}
            onPickScene={onPickScene}
          />
        ))}
      </div>

      {/* Skeletons rather than a spinner: they occupy the space the arriving
          cards will, so the page does not jump when they land. */}
      {pending && (
        <div className="scene-grid trending-skeletons" aria-hidden="true">
          {Array.from({ length: Math.min(PER_PAGE, 8) }, (_, i) => (
            <div className="scene-card skeleton-card" key={i}>
              <div className="skeleton-thumb" />
              <div className="skeleton-line" />
              <div className="skeleton-line short" />
            </div>
          ))}
        </div>
      )}

      {err && (
        <div className="trending-end">
          <p className="muted">{err}</p>
          <button
            type="button"
            className="grab-action match"
            onClick={() => {
              setErr("");
              setHasMore(true);
              void loadMore();
            }}
          >
            Try again
          </button>
        </div>
      )}

      {!hasMore && !err && scenes.length > 0 && (
        <p className="trending-end muted">
          That is the end of the ranking. {scenes.length} scenes.
        </p>
      )}

      {/* The sentinel sits below everything; crossing it asks for the next
          page. Kept out of the grid so it is never a grid cell. */}
      <div ref={sentinel} className="trending-sentinel" />
    </div>
  );
}
