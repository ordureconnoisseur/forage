import { useEffect, useState } from "react";
import PerformersList from "./views/PerformersList";
import MissingScenes from "./views/MissingScenes";
import SceneReleases from "./views/SceneReleases";
import GrabsList from "./views/GrabsList";
import DiscoverList from "./views/DiscoverList";
import Settings from "./views/Settings";
import { fetchHealth, Health, mixedContentBlocked } from "./api";

// URL-hash routes, parsed by parseRoute. The scene route carries the
// performer name in a query string so /grab can tell the placer which
// folder to use (forage owns final file placement now). The grabs
// route is a sibling of performers — accessible from the top nav and
// surfaces the full state machine the poller advances.
type Route =
  | { kind: "performers" }
  | { kind: "missing"; performerId: string }
  | { kind: "scene"; sceneId: string; performerName?: string }
  | { kind: "discover" }
  | { kind: "grabs" };

function parseRoute(hash: string): Route {
  const raw = hash.replace(/^#\/?/, "");
  const [pathPart, queryPart] = raw.split("?");
  const parts = pathPart.split("/").filter(Boolean);
  const query = new URLSearchParams(queryPart || "");
  if (parts[0] === "performer" && parts[1]) {
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
  if (parts[0] === "discover") {
    return { kind: "discover" };
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

  useEffect(() => {
    const onHash = () => setRoute(parseRoute(location.hash));
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // Poll /healthz so the first-run banner flips off as soon as the
  // user finishes configuring + saves. Refresh once on mount, then
  // whenever the Settings modal closes (cheap; one request).
  useEffect(() => {
    let cancelled = false;
    fetchHealth()
      .then((h) => {
        if (!cancelled) setHealth(h);
      })
      .catch(() => {
        // Silent — banner stays neutral when the daemon is unreachable.
      });
    return () => {
      cancelled = true;
    };
  }, [settingsOpen]);

  const goPerformers = () => setHash("#/");
  const goDiscover = () => setHash("#/discover");
  const goGrabs = () => setHash("#/grabs");
  const goPerformer = (id: string) => setHash(`#/performer/${id}`);
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
            href="#/grabs"
            onClick={(e) => {
              e.preventDefault();
              goGrabs();
            }}
            className={route.kind === "grabs" ? "active" : ""}
          >
            Grabs
          </a>
        </nav>
        <Breadcrumb route={route} onPerformers={goPerformers} />
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
          ⚠ Mixed content: this page is HTTPS but the forager URL is HTTP. The
          browser will block all API requests. Click the gear to set an HTTPS
          URL, or open Stash via a non-HTTPS URL.
        </div>
      )}
      {health?.unconfigured && (
        <div className="banner banner-setup">
          🌱 Welcome to Forage — your daemon needs credentials before it can
          do anything.{" "}
          <button
            className="banner-action"
            onClick={() => setSettingsOpen(true)}
          >
            Open Settings
          </button>
        </div>
      )}
      <main className="app-main">
        {!health?.unconfigured && route.kind === "performers" && (
          <PerformersList onPick={goPerformer} />
        )}
        {!health?.unconfigured && route.kind === "missing" && (
          <MissingScenes performerId={route.performerId} onPickScene={goScene} />
        )}
        {!health?.unconfigured && route.kind === "scene" && (
          <SceneReleases
            sceneId={route.sceneId}
            performerName={route.performerName}
          />
        )}
        {!health?.unconfigured && route.kind === "discover" && (
          <DiscoverList onPickPerformer={goPerformer} />
        )}
        {!health?.unconfigured && route.kind === "grabs" && <GrabsList />}
        {health?.unconfigured && (
          <div className="empty">
            Forage can't load any data until Stash + StashDB credentials are
            configured. Use the Settings button above to get started.
          </div>
        )}
      </main>
      {settingsOpen && (
        <Settings onClose={() => setSettingsOpen(false)} health={health} />
      )}
    </div>
  );
}

function Breadcrumb({
  route,
  onPerformers,
}: {
  route: Route;
  onPerformers: () => void;
}) {
  // Breadcrumb only renders for sub-routes under Performers. Top-level
  // routes (performers list, grabs list) get their nav highlight from
  // the .top-nav element instead.
  if (route.kind !== "missing" && route.kind !== "scene") return null;
  return (
    <nav className="breadcrumb">
      <a
        href="#/"
        onClick={(e) => {
          e.preventDefault();
          onPerformers();
        }}
      >
        Performers
      </a>
      <span className="sep">›</span>
      <span>
        {route.kind === "missing" ? "Missing scenes" : "Scene releases"}
      </span>
    </nav>
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
