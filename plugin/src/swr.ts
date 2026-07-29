import { useCallback, useEffect, useRef, useState } from "react";

// Stale-while-revalidate cache for the list views.
//
// App renders routes as `{route.kind === "x" && <View/>}`, so every
// navigation UNMOUNTS the previous view and destroys whatever it had
// fetched. Going back then remounted, flipped loading=true, and refetched
// from scratch behind a full-screen spinner — the "it has to load
// everything again" feeling, even when the data was seconds old.
//
// This keeps the last response per key in a module-level map, outside React,
// so it survives unmount. A remount paints the cached data on the FIRST
// render (no spinner, no flash of empty state) and revalidates in the
// background, swapping in fresh data if it differs. First-ever visit still
// shows a spinner, because there is genuinely nothing to show.
//
// Deliberately not a general query library: no dedup across components, no
// focus revalidation, no GC. The app has a handful of list endpoints and a
// single consumer each; anything more would be machinery without a problem.

interface Entry {
  data: unknown;
  at: number; // epoch ms of the response this came from
}

const cache = new Map<string, Entry>();

// Revalidation is skipped for a moment after a write, so bouncing between
// two views doesn't fire the same request twice in a second. Long enough to
// cover a double-click or a fast there-and-back, short enough that data is
// never perceptibly stale.
const FRESH_MS = 2000;

// invalidate drops keys so the next read refetches. Pass a prefix to clear a
// family ("/watches" clears "/watches?..." too). Call after any mutation
// whose effect the cached list would otherwise hide.
export function invalidate(prefix: string): void {
  for (const k of [...cache.keys()]) {
    if (k === prefix || k.startsWith(prefix)) cache.delete(k);
  }
}

// clearCache drops everything. Used when the identity behind the data
// changes (logout, or pointing the plugin at a different daemon), where
// serving another session's cached list would be plainly wrong.
export function clearCache(): void {
  cache.clear();
}

// peek/store are the same cache without the hook, for views that already own
// their fetching (WatchingList polls on a timer, and replacing that with
// useCached would mean two schedulers fighting). Seeding initial state from
// peek is what stops a remount painting empty; store keeps what the poll
// fetched available to the next mount.
export function peek<T>(key: string): T | null {
  const hit = cache.get(key);
  return hit ? (hit.data as T) : null;
}

export function store<T>(key: string, data: T): void {
  cache.set(key, { data, at: Date.now() });
}

export interface Cached<T> {
  data: T | null;
  // True only when there is nothing to paint yet. A background revalidation
  // over cached data leaves this false — that is the entire point.
  loading: boolean;
  // Set when a fetch failed. Kept alongside stale data rather than replacing
  // it: a failed refresh should not blank a working screen.
  error: string | null;
  // Refetch now, bypassing the freshness window. Resolves when done, so a
  // manual refresh button can await it before clearing its own spinner.
  reload: () => Promise<void>;
}

// useCached reads `key` from the cache and keeps it fresh.
//
// `key` doubles as the cache key and the identity of the request, so it must
// include every parameter that changes the response (sort, filters). A null
// key disables the hook entirely, for views that must not fetch yet.
export function useCached<T>(
  key: string | null,
  fetcher: (signal?: AbortSignal) => Promise<T>,
): Cached<T> {
  const initial = key ? (cache.get(key) as Entry | undefined) : undefined;
  const [data, setData] = useState<T | null>(
    initial ? (initial.data as T) : null,
  );
  const [loading, setLoading] = useState(!initial && !!key);
  const [error, setError] = useState<string | null>(null);

  // The fetcher is typically an inline arrow, so a new identity every render.
  // Holding it in a ref keeps it out of the effect's dependency list, which
  // would otherwise refetch on every render.
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  // Guards writes from a response whose key is no longer the one we want:
  // switching sort twice fast leaves the slower first response in flight,
  // and without this it would land after (and overwrite) the newer one.
  const seq = useRef(0);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const run = useCallback(
    async (force: boolean) => {
      if (!key) return;
      const hit = cache.get(key);
      if (hit && !force && Date.now() - hit.at < FRESH_MS) return;
      const mine = ++seq.current;
      if (!hit) setLoading(true);
      try {
        const fresh = await fetcherRef.current();
        // The sequence check gates the CACHE as well as the state. Writing
        // first meant a reload() that overtook an in-flight mount fetch left
        // the older payload in the cache: state was right, but the next
        // remount painted the stale entry.
        if (!alive.current || mine !== seq.current) return;
        cache.set(key, { data: fresh, at: Date.now() });
        setData(fresh);
        setError(null);
      } catch (e) {
        if (!alive.current || mine !== seq.current) return;
        // Leave `data` alone: stale content beats an error page.
        setError((e as Error).message);
      } finally {
        if (alive.current && mine === seq.current) setLoading(false);
      }
    },
    [key],
  );

  // Paint from cache synchronously when the key changes, so switching sort
  // (or coming back to a view) never flashes empty before the effect runs.
  const lastKey = useRef(key);
  if (key !== lastKey.current) {
    lastKey.current = key;
    const hit = key ? cache.get(key) : undefined;
    setData(hit ? (hit.data as T) : null);
    setLoading(!hit && !!key);
    setError(null);
  }

  useEffect(() => {
    void run(false);
  }, [run]);

  const reload = useCallback(() => run(true), [run]);
  return { data, loading, error, reload };
}
