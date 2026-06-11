import { useEffect, useRef, useState } from "react";
import PerformersList from "./views/PerformersList";
import MissingScenes from "./views/MissingScenes";
import CollectionMode from "./views/CollectionMode";
import SceneReleases from "./views/SceneReleases";
import GrabsList from "./views/GrabsList";
import JobsList from "./views/JobsList";
import WatchingList from "./views/WatchingList";
import DiscoverList from "./views/DiscoverList";
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
  startCollectionJob,
  verifyToken,
} from "./api";

// URL-hash routes, parsed by parseRoute. The scene route carries the
// performer name in a query string so /grab can tell the placer which
// folder to use (forage owns final file placement now). The grabs
// route is a sibling of performers — accessible from the top nav and
// surfaces the full state machine the poller advances.
type Route =
  | { kind: "performers" }
  | { kind: "missing"; performerId: string }
  | { kind: "collection"; performerId: string }
  | { kind: "scene"; sceneId: string; performerName?: string }
  | { kind: "discover" }
  | { kind: "watching" }
  | { kind: "grabs" }
  | { kind: "jobs" }
  | { kind: "job"; jobId: string; performerId: string };

function parseRoute(hash: string): Route {
  const raw = hash.replace(/^#\/?/, "");
  const [pathPart, queryPart] = raw.split("?");
  const parts = pathPart.split("/").filter(Boolean);
  const query = new URLSearchParams(queryPart || "");
  if (parts[0] === "performer" && parts[1]) {
    if (parts[2] === "collection") {
      return { kind: "collection", performerId: parts[1] };
    }
    return { kind: "missing", performerId: parts[1] };
  }
  if (parts[0] === "scene" && parts[1]) {
    return {
      kind: "scene",
      sceneId: parts[1],
      performerName: query.get("p") || undefined,
    };
  }
  if (parts[0] === "grabs") {
    return { kind: "grabs" };
  }
  if (parts[0] === "jobs") {
    return { kind: "jobs" };
  }
  // #/job/<jobId>/<performerId> — re-open a finished job as the
  // interactive collection view (performerId carried for folder context).
  if (parts[0] === "job" && parts[1] && parts[2]) {
    return { kind: "job", jobId: parts[1], performerId: parts[2] };
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
  // Optional scene-id subset for the next collection view, set when the
  // user launches "grab selected" from MissingScenes. In-memory (not in
  // the hash) since it's a transient list; cleared when it no longer
  // matches the active performer so a plain collection link is unscoped.
  const [collectionScope, setCollectionScope] = useState<{
    performerId: string;
    sceneIds: string[];
  } | null>(null);
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
  // foragerBase() is read once per render; useEffect below re-runs
  // when settings open/close so a Save updates the URL → triggers a
  // fresh health probe.
  const apiURL = foragerBase();

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
    fetchHealth()
      .then(async (h) => {
        if (cancelled) return;
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
        if (cancelled) return;
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

  const goPerformers = () => setHash("#/");
  const goDiscover = () => setHash("#/discover");
  const goWatching = () => setHash("#/watching");
  const goGrabs = () => setHash("#/grabs");
  const goJobs = () => setHash("#/jobs");
  const goJobReview = (jobId: string, performerId: string) =>
    setHash(`#/job/${jobId}/${performerId}`);
  const goPerformer = (id: string) => setHash(`#/performer/${id}`);
  const goCollection = (id: string) => {
    setCollectionScope(null); // full collection
    setHash(`#/performer/${id}/collection`);
  };
  // Launch the collection view scoped to a hand-picked subset of scenes.
  const goCollectionSelected = (id: string, sceneIds: string[]) => {
    setCollectionScope({ performerId: id, sceneIds });
    setHash(`#/performer/${id}/collection`);
  };
  const goScene = (id: string, performerName?: string) => {
    const suffix = performerName
      ? `?p=${encodeURIComponent(performerName)}`
      : "";
    setHash(`#/scene/${id}${suffix}`);
  };
  // Hand a collection crawl to the daemon, then jump to the Jobs tab.
  const runCollectionOnServer = async (id: string, sceneIds?: string[]) => {
    try {
      await startCollectionJob(id, sceneIds);
    } catch (e) {
      // Surface minimally; the Jobs tab will show nothing if it failed.
      alert("Couldn't start server job: " + (e as Error).message);
      return;
    }
    goJobs();
  };
  // Grab a Discover selection. The collection flow is per-performer, so
  // a single-performer selection opens the same interactive collection
  // view the missing page uses; a multi-performer selection fans out one
  // server job per performer and lands on the Jobs tab for review.
  const grabDiscoverSelection = async (
    groups: Array<{ performerId: string; sceneIds: string[] }>,
  ) => {
    if (groups.length === 0) return;
    if (groups.length === 1) {
      goCollectionSelected(groups[0].performerId, groups[0].sceneIds);
      return;
    }
    const results = await Promise.allSettled(
      groups.map((g) => startCollectionJob(g.performerId, g.sceneIds)),
    );
    const failed = results.filter((r) => r.status === "rejected").length;
    if (failed > 0) {
      alert(
        `Started ${groups.length - failed} of ${groups.length} server jobs — ` +
          `${failed} failed. The Jobs tab shows what's running.`,
      );
    }
    goJobs();
  };
  // Start an UPGRADE crawl over owned scenes (sceneIds = the selection, or
  // omitted = every owned scene) and jump straight to its review — the job
  // only suggests releases that beat each scene's current resolution.
  const goUpgrade = async (id: string, sceneIds?: string[]) => {
    try {
      const job = await startCollectionJob(id, sceneIds, { upgrade: true });
      goJobReview(job.id, id);
    } catch (e) {
      alert("Couldn't start upgrade job: " + (e as Error).message);
    }
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
              route.kind === "collection" ||
              route.kind === "scene"
                ? "active"
                : ""
            }
          >
            <NavIcon name="performers" />
            Performers
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
                {notif.watches_available > 9 ? "9+" : notif.watches_available}
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
          <a
            href="#/jobs"
            onClick={(e) => {
              e.preventDefault();
              goJobs();
            }}
            className={route.kind === "jobs" ? "active" : ""}
          >
            <NavIcon name="jobs" />
            Jobs
          </a>
        </nav>
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
          ⚠ Mixed content: this page is HTTPS but the forage URL is HTTP. The
          browser will block all API requests. Click the gear to set an HTTPS
          URL, or open Stash via a non-HTTPS URL.
        </div>
      )}
      <main className="app-main">
        {ready && route.kind === "performers" && (
          <PerformersList onPick={goPerformer} />
        )}
        {ready && route.kind === "missing" && (
          <MissingScenes
            performerId={route.performerId}
            onPickScene={goScene}
            onCollection={goCollection}
            onGrabSelected={goCollectionSelected}
            onUpgrade={goUpgrade}
          />
        )}
        {ready && route.kind === "collection" && (
          <CollectionMode
            performerId={route.performerId}
            onBack={goPerformer}
            onRunOnServer={runCollectionOnServer}
            sceneIds={
              collectionScope &&
              collectionScope.performerId === route.performerId
                ? collectionScope.sceneIds
                : undefined
            }
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
            onGrabSelected={grabDiscoverSelection}
          />
        )}
        {ready && route.kind === "watching" && (
          <WatchingList onPickScene={goScene} />
        )}
        {ready && route.kind === "grabs" && (
          <GrabsList onPickScene={goScene} />
        )}
        {ready && route.kind === "jobs" && (
          <JobsList onPickPerformer={goPerformer} onReview={goJobReview} />
        )}
        {ready && route.kind === "job" && (
          <CollectionMode
            performerId={route.performerId}
            jobId={route.jobId}
            onBack={() => goJobs()}
            onRunOnServer={runCollectionOnServer}
          />
        )}
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

