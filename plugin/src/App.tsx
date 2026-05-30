import { useEffect, useState } from "react";
import PerformersList from "./views/PerformersList";
import MissingScenes from "./views/MissingScenes";
import CollectionMode from "./views/CollectionMode";
import SceneReleases from "./views/SceneReleases";
import GrabsList from "./views/GrabsList";
import JobsList from "./views/JobsList";
import WatchingList from "./views/WatchingList";
import DiscoverList from "./views/DiscoverList";
import Settings from "./views/Settings";
import {
  fetchHealth,
  foragerBase,
  Health,
  mixedContentBlocked,
  startCollectionJob,
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
  // Non-null when /healthz failed to load — daemon URL set but
  // unreachable, or returning HTML (wrong host, plugin pointed at
  // Stash itself, etc.). Distinguished from "unconfigured daemon"
  // which still returns valid JSON with `unconfigured: true`.
  const [healthError, setHealthError] = useState<string | null>(null);
  // foragerBase() is read once per render; useEffect below re-runs
  // when settings open/close so a Save updates the URL → triggers a
  // fresh health probe.
  const apiURL = foragerBase();

  useEffect(() => {
    const onHash = () => setRoute(parseRoute(location.hash));
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // Probe /healthz on mount + after Settings closes. When the URL
  // isn't set yet, skip the fetch (it'd hit Stash's origin and
  // JSON.parse-fail on Stash's HTML index — that's the bug this
  // guard fixes). Setup banner picks up the empty state instead.
  useEffect(() => {
    if (!apiURL) {
      setHealth(null);
      setHealthError(null);
      return;
    }
    let cancelled = false;
    fetchHealth()
      .then((h) => {
        if (cancelled) return;
        setHealth(h);
        setHealthError(null);
      })
      .catch((e) => {
        if (cancelled) return;
        setHealthError((e as Error).message);
        setHealth(null);
      });
    return () => {
      cancelled = true;
    };
  }, [settingsOpen, apiURL]);

  // Three distinct "needs setup" states, each with its own banner copy:
  //   - no URL → user has never configured (or localStorage was cleared)
  //   - URL set but unreachable → daemon down, wrong URL, mixed-content
  //   - URL set, healthz OK, but Stash creds missing → unconfigured daemon
  const needsSetup =
    !apiURL || !!healthError || health?.unconfigured === true;

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
            Watching
          </a>
          <a
            href="#/grabs"
            onClick={(e) => {
              e.preventDefault();
              goGrabs();
            }}
            className={route.kind === "grabs" ? "active" : ""}
          >
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
            Jobs
          </a>
        </nav>
        <button
          className="header-settings"
          onClick={() => setSettingsOpen(true)}
          title="Settings"
          aria-label="Settings"
        >
          <GearIcon />
        </button>
      </header>
      {blocked && (
        <div className="banner banner-warn">
          ⚠ Mixed content: this page is HTTPS but the forage URL is HTTP. The
          browser will block all API requests. Click the gear to set an HTTPS
          URL, or open Stash via a non-HTTPS URL.
        </div>
      )}
      {needsSetup && (
        <div className="banner banner-setup">
          {!apiURL ? (
            <>
              🌱 Forage API URL isn't set. Point the plugin at your daemon to
              get started.{" "}
            </>
          ) : healthError ? (
            <>
              ⚠ Can't reach the forage daemon at <code>{apiURL}</code> —{" "}
              {healthError}.{" "}
            </>
          ) : (
            <>
              🌱 Welcome to Forage — your daemon needs credentials before it can
              do anything.{" "}
            </>
          )}
          <button
            className="banner-action"
            onClick={() => setSettingsOpen(true)}
          >
            Open Settings
          </button>
        </div>
      )}
      <main className="app-main">
        {!needsSetup && route.kind === "performers" && (
          <PerformersList onPick={goPerformer} />
        )}
        {!needsSetup && route.kind === "missing" && (
          <MissingScenes
            performerId={route.performerId}
            onPickScene={goScene}
            onCollection={goCollection}
            onGrabSelected={goCollectionSelected}
          />
        )}
        {!needsSetup && route.kind === "collection" && (
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
        {!needsSetup && route.kind === "scene" && (
          <SceneReleases
            sceneId={route.sceneId}
            performerName={route.performerName}
          />
        )}
        {!needsSetup && route.kind === "discover" && (
          <DiscoverList onPickPerformer={goPerformer} onPickScene={goScene} />
        )}
        {!needsSetup && route.kind === "watching" && (
          <WatchingList onPickScene={goScene} />
        )}
        {!needsSetup && route.kind === "grabs" && <GrabsList />}
        {!needsSetup && route.kind === "jobs" && (
          <JobsList onPickPerformer={goPerformer} onReview={goJobReview} />
        )}
        {!needsSetup && route.kind === "job" && (
          <CollectionMode
            performerId={route.performerId}
            jobId={route.jobId}
            onBack={() => goJobs()}
            onRunOnServer={runCollectionOnServer}
          />
        )}
        {needsSetup && (
          <div className="empty">
            {!apiURL
              ? "Set the Forage API URL in Settings to get started."
              : healthError
                ? "Check your Forage API URL and that the daemon is running."
                : "Forage can't load any data until Stash + StashDB credentials are configured. Use the Settings button above to get started."}
          </div>
        )}
      </main>
      {settingsOpen && (
        <Settings onClose={() => setSettingsOpen(false)} health={health} />
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

/* Acorn glyph from SVG Repo (acorn-1-svgrepo-com), recoloured to
   currentColor so the leafy-green --accent flows through. Original
   viewBox 0 0 512 512, two-path composite (cap + nut). */
function AcornIcon() {
  return (
    <span className="brand-icon" aria-hidden="true">
      <svg viewBox="0 0 512 512" xmlns="http://www.w3.org/2000/svg">
        <path d="M68.173,182.354c-98.206,98.221-73.314,248.837-30.337,291.827c43.005,43.006,193.634,67.898,291.84-30.323c51.09-51.075,90.54-90.525,90.54-90.525L158.711,91.83C158.711,91.83,119.263,131.279,68.173,182.354z M194.604,412.66c22.309-4.264,44.728-14.015,63.814-33.059c8.334-8.334,21.835-8.334,30.17,0c8.334,8.334,8.334,21.837,0,30.17c-25.949,25.99-57.035,39.352-86.039,44.826c-29.114,5.472-56.284,3.472-77.259-1.417c-11.474-2.71-18.586-14.183-15.891-25.656c2.696-11.474,14.196-18.586,25.67-15.891C150.404,415.272,172.379,416.924,194.604,412.66z" />
        <path d="M456.595,96.803l22.837-22.851c10.194-10.196,10.194-26.697,0-36.878c-10.169-10.183-26.698-10.196-36.866,0l-22.35,22.336C336.065-12.697,235.721-15.1,190.27,30.35c-20.057,20.044-22.835,22.837-23.141,23.142l291.41,291.41c0.306-0.306,3.084-3.084,23.142-23.142C526.242,277.2,524.631,179.909,456.595,96.803z" />
      </svg>
    </span>
  );
}
