import { WarnIcon } from "./icons";
import { useEffect, useRef, useState } from "react";
import PerformersList from "./views/PerformersList";
import StudiosList from "./views/StudiosList";
import MissingScenes from "./views/MissingScenes";
import SceneReleases from "./views/SceneReleases";
import GrabsList from "./views/GrabsList";
import DeletionsList from "./views/DeletionsList";
import UnfiledList from "./views/UnfiledList";
import WatchingList from "./views/WatchingList";
import DiscoverList from "./views/DiscoverList";
import TrendingList from "./views/TrendingList";
import Setup from "./views/Setup";
import Settings from "./views/Settings";
import Login from "./views/Login";
import AcornIcon from "./AcornIcon";
import NavIcon from "./NavIcons";
import NotificationsBell from "./NotificationsBell";
import {
  establishSession,
  fetchHealth,
  fetchNotifications,
  foragerBase,
  Health,
  mixedContentBlocked,
  NotificationCounts,
  setUnauthorizedHandler,
  verifyToken,
} from "./api";

// Panic banner: only a crash from the last 48h is worth interrupting for
// (older ones are still in /diag), and a dismiss remembers the dismissed
// panic's timestamp so only a NEW crash re-raises it.
const PANIC_DISMISS_KEY = "forage.panicDismissedAt";
const PANIC_BANNER_WINDOW_SEC = 48 * 3600;

// panicAge renders a unix timestamp as a rough "how long ago" phrase.
function panicAge(at: number): string {
  const h = Math.floor((Date.now() / 1000 - at) / 3600);
  if (h < 1) return "less than an hour ago";
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// URL-hash routes, parsed by parseRoute. The scene route carries the
// performer name in a query string so /grab can tell the placer which
// folder to use (forage owns final file placement now). The grabs
// route is a sibling of performers — accessible from the top nav and
// surfaces the full state machine the poller advances.
type Route =
  | { kind: "performers" }
  | { kind: "missing"; performerId: string }
  | { kind: "studios" }
  | { kind: "studio"; studioId: string }
  | { kind: "scene"; sceneId: string; performerName?: string }
  | { kind: "discover" }
  // Trending past the carousel: the source's global ranking, browsed
  // live. Reachable from Discover rather than the nav, like unfiled.
  | { kind: "trending"; box?: string }
  | { kind: "watching" }
  // grabs carries an optional starting query so other views can link into
  // a filtered Grabs (the Watching tab points at a batch's finished work,
  // whose real download state lives here rather than on the watch).
  | { kind: "grabs"; q?: string }
  | { kind: "unfiled" }
  | { kind: "deletions" };

function parseRoute(hash: string): Route {
  const raw = hash.replace(/^#\/?/, "");
  const [pathPart, queryPart] = raw.split("?");
  const parts = pathPart.split("/").filter(Boolean);
  const query = new URLSearchParams(queryPart || "");
  if (parts[0] === "performer" && parts[1]) {
    return { kind: "missing", performerId: parts[1] };
  }
  // Studio detail before the bare /studios list. The id is a StashDB cross-id
  // (or a synthetic "stash:<id>"), url-encoded in the hash.
  if (parts[0] === "studio" && parts[1]) {
    return { kind: "studio", studioId: decodeURIComponent(parts[1]) };
  }
  if (parts[0] === "studios") {
    return { kind: "studios" };
  }
  if (parts[0] === "scene" && parts[1]) {
    return {
      kind: "scene",
      sceneId: parts[1],
      performerName: query.get("p") || undefined,
    };
  }
  if (parts[0] === "grabs") {
    return { kind: "grabs", q: query.get("q") || undefined };
  }
  if (parts[0] === "unfiled") {
    return { kind: "unfiled" };
  }
  if (parts[0] === "deletions") {
    return { kind: "deletions" };
  }
  if (parts[0] === "trending") {
    return { kind: "trending", box: query.get("box") || undefined };
  }
  if (parts[0] === "discover") {
    return { kind: "discover" };
  }
  if (parts[0] === "watching") {
    return { kind: "watching" };
  }
  return { kind: "performers" };
}

function setHash(h: string) {
  if (location.hash !== h) location.hash = h;
}

export default function App() {
  const [route, setRoute] = useState<Route>(parseRoute(location.hash));
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [health, setHealth] = useState<Health | null>(null);
  // Bumped by the setup wizard on completion to force a fresh health
  // probe (so needsSetup re-evaluates and the wizard unmounts) without a
  // full page reload.
  const [healthNonce, setHealthNonce] = useState(0);
  // Non-null when /healthz failed to load — daemon URL set but
  // unreachable, or returning HTML (wrong host, plugin pointed at
  // Stash itself, etc.). Distinguished from "unconfigured daemon"
  // which still returns valid JSON with `unconfigured: true`.
  const [healthError, setHealthError] = useState<string | null>(null);
  // False until the first /healthz probe resolves, so we show a brief
  // loading splash instead of flashing the setup wizard before we know
  // whether the daemon is reachable.
  const [healthProbed, setHealthProbed] = useState(false);
  // Whether we hold a valid admin token, when the daemon requires one.
  // null = not yet determined (verifying); true = a gated probe succeeded;
  // false = no/invalid token → show the Login gate. Irrelevant (left at
  // true) when the daemon doesn't require auth.
  const [authOk, setAuthOk] = useState<boolean | null>(null);
  // The panic banner nags per-panic, not forever: dismissing remembers the
  // dismissed panic's timestamp, so the same crash stays hidden but a NEW
  // one (different timestamp) re-raises the banner.
  const [dismissedPanicAt, setDismissedPanicAt] = useState<number>(() =>
    Number(localStorage.getItem(PANIC_DISMISS_KEY) ?? 0),
  );
  // foragerBase() is read once per render; useEffect below re-runs
  // when settings open/close so a Save updates the URL → triggers a
  // fresh health probe.
  const apiURL = foragerBase();

  // healthSeq orders /healthz responses across the two writers (the
  // mount/settings probe and the 30s re-poll): each fetch takes a
  // ticket, and only the response holding the newest ticket may write
  // health state. Without it a stalled older response resolving after a
  // newer one would overwrite fresh data (e.g. re-installing a
  // resolved-outage banner), which is realistic under exactly the flaky
  // network conditions the reachability banner exists for.
  const healthSeq = useRef(0);

  useEffect(() => {
    const onHash = () => setRoute(parseRoute(location.hash));
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // Establish the forage_token cookie so <img> requests (performer
  // portraits, scene screenshots) authenticate without a bearer header.
  // No-op when no admin token is set. Re-run when the daemon URL or token
  // changes (healthNonce bumps after a Settings/Setup save).
  useEffect(() => {
    establishSession();
  }, [apiURL, healthNonce]);

  // Probe /healthz on mount + after Settings closes. We always probe —
  // an empty base means "same origin", which is correct when the daemon
  // serves the standalone app at /. The probe hits <origin>/healthz; if a
  // forage daemon answers we're good (standalone or a configured URL), and
  // if it fails (e.g. the Vite dev server, or an unreachable configured
  // URL) we fall through to the setup wizard's connect step.
  useEffect(() => {
    let cancelled = false;
    const seq = ++healthSeq.current;
    fetchHealth()
      .then(async (h) => {
        if (cancelled || seq !== healthSeq.current) return;
        setHealth(h);
        setHealthError(null);
        // Resolve the auth phase before flipping healthProbed (below, in
        // finally) so we never flash the app or the login gate: an
        // auth-required daemon gets a silent gated probe; an open one is
        // authed by definition.
        if (!h.adminAuthRequired) {
          setAuthOk(true);
        } else {
          const ok = await verifyToken();
          if (!cancelled) setAuthOk(ok);
        }
      })
      .catch((e) => {
        if (cancelled || seq !== healthSeq.current) return;
        setHealthError((e as Error).message);
        setHealth(null);
        setAuthOk(null);
      })
      .finally(() => {
        if (!cancelled) setHealthProbed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [settingsOpen, apiURL, healthNonce]);

  // Any API call returning 401 (token revoked/rotated, cookie expired)
  // fires this — even calls whose own error handling is silent — so a
  // mid-session lockout bounces to the Login gate instead of leaving
  // broken/empty views. needsLogin (below) gates on authOk === false
  // AND the health probe agreeing auth is required — and the cached
  // health may still say it isn't (a password was set from another
  // browser mid-session, rotating the session key). Re-probe health on
  // 401 so the gate can actually engage; without it every view just
  // showed errors until a full page reload.
  const lastAuthProbe = useRef(0);
  useEffect(() => {
    setUnauthorizedHandler(() => {
      setAuthOk(false);
      // Debounced: bumping the nonce re-runs the health probe AND the
      // effects that depend on it (the notifications poll refetches
      // immediately), so an environment where API calls 401 while
      // /healthz still says open would otherwise loop probe → fetch →
      // 401 → probe at network speed. One re-probe per 10s is plenty to
      // route a genuine mid-session lockout to the Login gate.
      const now = Date.now();
      if (now - lastAuthProbe.current > 10_000) {
        lastAuthProbe.current = now;
        setHealthNonce((n) => n + 1);
      }
    });
    return () => setUnauthorizedHandler(null);
  }, []);

  // Drive the top-level phase off the probe result, not URL presence:
  //   - loading  → first probe hasn't resolved yet
  //   - login    → daemon reachable + requires a token we don't hold
  //   - setup    → daemon unreachable at this origin / no configured URL,
  //                or reachable but missing Stash creds (unconfigured)
  //   - ready    → daemon reachable + configured + authed
  //
  // Login precedes setup: a configured-but-locked daemon can't be
  // configured without auth, so the gate wins even when unconfigured.
  const loading = !healthProbed;
  const needsLogin =
    healthProbed &&
    !healthError &&
    health?.adminAuthRequired === true &&
    authOk === false;
  const needsSetup =
    healthProbed &&
    !needsLogin &&
    (!!healthError || health?.unconfigured === true);
  const ready = healthProbed && !needsLogin && !needsSetup;
  // Same-origin: an empty base reached a live daemon → the daemon is
  // serving us standalone, so the setup wizard can skip its "connect to
  // your daemon" step entirely.
  const sameOrigin = apiURL === "" && !healthError && health != null;

  // Poll the actionable-notification counts for the header bell + the
  // Watching-tab badge. Light (cheap counts), every 45s, only once the
  // daemon is reachable + configured. Re-runs on route change so acting
  // on something (grabbing a ready watch) reflects promptly.
  const [notif, setNotif] = useState<NotificationCounts | null>(null);
  useEffect(() => {
    if (!ready) {
      setNotif(null);
      return;
    }
    let cancelled = false;
    const load = () =>
      fetchNotifications()
        .then((n) => {
          if (!cancelled) setNotif(n);
        })
        .catch(() => {});
    load();
    const id = window.setInterval(load, 45000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [ready, healthNonce, route]);

  // Re-poll /healthz while the app is running so the download-client
  // reachability banner tracks an outage that starts mid-session (a VPN
  // blip taking qBit offline). Updates only on success and never touches
  // healthError/authOk, so a transient fetch failure can't bounce a
  // working session to the setup wizard the way the initial probe's
  // failure path does. Note a successful payload replaces the WHOLE
  // health object: if the daemon genuinely becomes unconfigured
  // mid-session (config store wiped, creds cleared elsewhere), needsSetup
  // flips and the app routes to Setup within one tick. That is intended
  // routing, since every data fetch is broken at that point anyway; the
  // pre-poll app did the same on the next healthNonce bump, just later.
  // healthSeq keeps a stalled older response from overwriting a newer
  // one (see its declaration above).
  useEffect(() => {
    if (!ready) return;
    let cancelled = false;
    const id = window.setInterval(() => {
      const seq = ++healthSeq.current;
      fetchHealth()
        .then((h) => {
          if (!cancelled && seq === healthSeq.current) setHealth(h);
        })
        .catch(() => {});
    }, 30000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [ready, healthNonce]);

  const goPerformers = () => setHash("#/");
  const goStudios = () => setHash("#/studios");
  const goDiscover = () => setHash("#/discover");
  const goTrending = (box?: string) =>
    setHash("#/trending" + (box ? "?box=" + encodeURIComponent(box) : ""));
  const goWatching = () => setHash("#/watching");
  // Bare for the nav button; with a query when another view links into a
  // filtered Grabs (see the Watching tab's finished-work disclosure).
  const goGrabs = (q?: string) =>
    setHash(q ? `#/grabs?q=${encodeURIComponent(q)}` : "#/grabs");
  const goPerformer = (id: string) => setHash(`#/performer/${id}`);
  const goStudio = (id: string) => setHash(`#/studio/${encodeURIComponent(id)}`);
  const goScene = (id: string, performerName?: string) => {
    const suffix = performerName
      ? `?p=${encodeURIComponent(performerName)}`
      : "";
    setHash(`#/scene/${id}${suffix}`);
  };

  const blocked = mixedContentBlocked();

  return (
    <div className="app">
      <header className="app-header">
        <a
          href="#/"
          onClick={(e) => {
            e.preventDefault();
            goPerformers();
          }}
          className="brand"
        >
          <AcornIcon />
          forage
        </a>
        {/* Hidden while the gate is up. Five tabs that move their own active
            highlight and render nothing behind it are worse than no tabs: the
            brand lockup stays, because that is the one thing on the screen
            that should be there. */}
        {!needsLogin && !needsSetup && (
        <nav className="top-nav">
          <a
            href="#/"
            onClick={(e) => {
              e.preventDefault();
              goPerformers();
            }}
            className={
              route.kind === "performers" ||
              route.kind === "missing" ||
              route.kind === "scene"
                ? "active"
                : ""
            }
          >
            <NavIcon name="performers" />
            Performers
          </a>
          <a
            href="#/studios"
            onClick={(e) => {
              e.preventDefault();
              goStudios();
            }}
            className={
              route.kind === "studios" || route.kind === "studio"
                ? "active"
                : ""
            }
          >
            <NavIcon name="studios" />
            Studios
          </a>
          <a
            href="#/discover"
            onClick={(e) => {
              e.preventDefault();
              goDiscover();
            }}
            className={route.kind === "discover" ? "active" : ""}
          >
            <NavIcon name="discover" />
            Discover
          </a>
          <a
            href="#/watching"
            onClick={(e) => {
              e.preventDefault();
              goWatching();
            }}
            className={route.kind === "watching" ? "active" : ""}
          >
            <NavIcon name="watching" />
            Watching
            {notif && notif.watches_available > 0 && (
              <span className="nav-badge" title="Ready to grab">
                {notif.watches_available > 99 ? "99+" : notif.watches_available}
              </span>
            )}
          </a>
          <a
            href="#/grabs"
            onClick={(e) => {
              e.preventDefault();
              goGrabs();
            }}
            className={route.kind === "grabs" ? "active" : ""}
          >
            <NavIcon name="grabs" />
            Grabs
          </a>
        </nav>
        )}
        <div className="header-right">
          {ready && (
            <NotificationsBell
              counts={notif}
              onGoWatching={goWatching}
              onGoGrabs={goGrabs}
            />
          )}
          <button
            className="header-settings"
            onClick={() => setSettingsOpen(true)}
            title="Settings"
            aria-label="Settings"
          >
            <GearIcon />
          </button>
        </div>
      </header>
      {blocked && (
        <div className="banner banner-warn">
          <WarnIcon size={12} /> Mixed content: this page is HTTPS but the forage URL is HTTP. The
          browser will block all API requests. Click the gear to set an HTTPS
          URL, or open Stash via a non-HTTPS URL.
        </div>
      )}
      {health?.clientErrors && health.clientErrors.length > 0 && (
        <div className="banner banner-error" role="alert">
          <WarnIcon size={12} /> Download client unavailable: grabs will fail until it is
          reachable again.
          {health.clientErrors.map((e) => (
            <span key={e}>
              {" "}
              <code>{e}</code>
            </span>
          ))}
        </div>
      )}
      {health?.lastPanic &&
        health.lastPanic.at > dismissedPanicAt &&
        Date.now() / 1000 - health.lastPanic.at < PANIC_BANNER_WINDOW_SEC && (
          <div className="banner banner-warn" role="alert">
            <WarnIcon size={12} /> A background task crashed and recovered{" "}
            {panicAge(health.lastPanic.at)}{" "}
            (in <code>{health.lastPanic.in}</code>). The daemon is fine, but
            please report it, <code>/diag</code> has the details.{" "}
            <button
              className="banner-dismiss"
              onClick={() => {
                localStorage.setItem(
                  PANIC_DISMISS_KEY,
                  String(health.lastPanic!.at),
                );
                setDismissedPanicAt(health.lastPanic!.at);
              }}
              title="Hide until a new crash happens"
            >
              ×
            </button>
          </div>
        )}
      <main className="app-main">
        {ready && route.kind === "performers" && (
          <PerformersList onPick={goPerformer} />
        )}
        {ready && route.kind === "missing" && (
          <MissingScenes
            key={"performer:" + route.performerId}
            subject={{ kind: "performer", id: route.performerId }}
            onPickScene={goScene}
          />
        )}
        {ready && route.kind === "studios" && (
          <StudiosList onPick={goStudio} />
        )}
        {ready && route.kind === "studio" && (
          <MissingScenes
            key={"studio:" + route.studioId}
            subject={{ kind: "studio", id: route.studioId }}
            onPickScene={goScene}
          />
        )}
        {ready && route.kind === "scene" && (
          <SceneReleases
            key={route.sceneId}
            sceneId={route.sceneId}
            performerName={route.performerName}
          />
        )}
        {ready && route.kind === "discover" && (
          <DiscoverList
            onPickPerformer={goPerformer}
            onPickScene={goScene}
            onSeeTrending={goTrending}
          />
        )}
        {ready && route.kind === "trending" && (
          <TrendingList
            box={route.box || ""}
            onPickPerformer={goPerformer}
            onPickScene={goScene}
            onBack={goDiscover}
          />
        )}
        {ready && route.kind === "watching" && (
          <WatchingList onPickScene={goScene} onPickGrabs={goGrabs} />
        )}
        {ready && route.kind === "grabs" && (
          <GrabsList onPickScene={goScene} initialQ={route.q} />
        )}
        {ready && route.kind === "unfiled" && <UnfiledList />}
        {ready && route.kind === "deletions" && <DeletionsList />}
        {loading && (
          <div className="app-loading" role="status" aria-live="polite">
            <span className="coll-spinner" />
          </div>
        )}
        {needsLogin && (
          <Login
            onAuthed={() => setAuthOk(true)}
            passwordSet={!!health?.passwordSet}
          />
        )}
        {needsSetup && (
          <Setup
            health={health}
            healthError={healthError}
            sameOrigin={sameOrigin}
            onDone={() => setHealthNonce((n) => n + 1)}
            onAdvanced={() => setSettingsOpen(true)}
          />
        )}
      </main>
      {settingsOpen && (
        <Settings
          onClose={() => setSettingsOpen(false)}
          onLoggedOut={() => {
            setSettingsOpen(false);
            setAuthOk(false);
          }}
          health={health}
        />
      )}
    </div>
  );
}

/* Gear icon — same Lucide-style stroke gear refract injects into its
   custom settings sidebar (refract.js NAV_ICONS.settings). Inlined
   so the plugin looks identical with or without refract loaded. */
function GearIcon() {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 00.33 1.82l.06.06a2 2 0 01-2.83 2.83l-.06-.06a1.65 1.65 0 00-1.82-.33 1.65 1.65 0 00-1 1.51V21a2 2 0 01-4 0v-.09A1.65 1.65 0 009 19.4a1.65 1.65 0 00-1.82.33l-.06.06a2 2 0 01-2.83-2.83l.06-.06a1.65 1.65 0 00.33-1.82 1.65 1.65 0 00-1.51-1H3a2 2 0 010-4h.09A1.65 1.65 0 004.6 9a1.65 1.65 0 00-.33-1.82l-.06-.06a2 2 0 012.83-2.83l.06.06a1.65 1.65 0 001.82.33H9a1.65 1.65 0 001-1.51V3a2 2 0 014 0v.09a1.65 1.65 0 001 1.51 1.65 1.65 0 001.82-.33l.06-.06a2 2 0 012.83 2.83l-.06.06a1.65 1.65 0 00-.33 1.82V9a1.65 1.65 0 001.51 1H21a2 2 0 010 4h-.09a1.65 1.65 0 00-1.51 1z" />
    </svg>
  );
}

