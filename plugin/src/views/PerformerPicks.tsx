import { useEffect, useRef, useState } from "react";
import {
  addPerformerFromStashDB,
  fetchDiscoverPerformers,
  stashdbImageURL,
  type DiscoverPerformerPick,
  type DiscoverPerformersResponse,
} from "../api";
import { CheckIcon, PlusIcon } from "../icons";

// Who to follow next, at the top of the page listing who you already follow.
//
// Three lenses, one strip. They could have been three strips, but they answer
// one question from three angles and stacking them would push the library
// three screens down to make that point.
//
//   Trending   who recurs across the current trending SCENES. StashDB has no
//              trending sort for performers, so the daemon derives this.
//   New faces  StashDB's DEBUT sort: their first scene just landed.
//   Active     StashDB's LAST_SCENE sort: they released most recently.
//
// Everyone shown is someone the library does NOT have, so every card does the
// same single thing: add them. That is why the whole card is the button, the
// same grammar the "+ performer" chips on Discover use.

type Lens = "trending" | "debut" | "active";

const LENSES: { key: Lens; label: string; hint: string }[] = [
  {
    key: "trending",
    label: "Trending",
    hint: "On the most trending scenes right now",
  },
  { key: "debut", label: "New faces", hint: "Their first scene just landed" },
  { key: "active", label: "Active", hint: "Released most recently" },
];

export default function PerformerPicks() {
  const [data, setData] = useState<DiscoverPerformersResponse | null>(null);
  const [lens, setLens] = useState<Lens>("trending");
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    fetchDiscoverPerformers()
      .then((d) => !cancelled && setData(d))
      .catch(() => !cancelled && setFailed(true));
    return () => {
      cancelled = true;
    };
  }, []);

  // Nothing at all rather than an empty frame: this is a suggestion strip
  // above someone's actual library, and a broken-looking one is worse than
  // none. Same reason the subscriptions row hides itself.
  if (failed) return null;

  const picks = data ? data[lens] : [];
  const loading = !data;
  if (data && !data.trending.length && !data.debut.length && !data.active.length) {
    return null;
  }

  return (
    <section className="picks-section">
      <div className="picks-head">
        <h3 className="section-header">Performers to follow</h3>
        <div className="picks-lenses" role="tablist" aria-label="Lens">
          {LENSES.map((l) => (
            <button
              key={l.key}
              type="button"
              role="tab"
              aria-selected={lens === l.key}
              className="picks-lens"
              title={l.hint}
              onClick={() => setLens(l.key)}
            >
              {l.label}
            </button>
          ))}
        </div>
      </div>

      {/* A scroll strip rather than a paged carousel. The scene carousel pages
          because its cards are wide and few; these are narrow and there are
          twenty-four of them, so swiping beats clicking through five pages,
          and it costs no chevrons at any width. */}
      <div className="picks-row">
        {loading
          ? Array.from({ length: 8 }, (_, i) => (
              <div className="pick-card is-skeleton" key={i} aria-hidden="true" />
            ))
          : picks.map((p) => <PickCard key={p.stashdb_id} p={p} lens={lens} />)}
      </div>
    </section>
  );
}

function PickCard({ p, lens }: { p: DiscoverPerformerPick; lens: Lens }) {
  const [state, setState] = useState<"idle" | "adding" | "added" | "err">("idle");
  const [msg, setMsg] = useState("");
  const [noImage, setNoImage] = useState(false);
  const img = useRef(stashdbImageURL(p.image_url)).current;

  const add = async () => {
    if (state === "adding" || state === "added") return;
    setState("adding");
    try {
      const r = await addPerformerFromStashDB(p.stashdb_id, p.name);
      setState("added");
      setMsg(r.already_present ? "already in your library" : "added");
    } catch (e) {
      setState("err");
      setMsg((e as Error).message);
    }
  };

  // The stat says something different per lens, because the reason to look at
  // this person is different per lens. On trending it is the ranking signal
  // itself; elsewhere the ORDER carries the recency (StashDB exposes no debut
  // or last-scene DATE to print), so the scene count is what is left worth
  // saying.
  const stat =
    lens === "trending" && p.trending_scenes
      ? `on ${p.trending_scenes} trending scene${p.trending_scenes === 1 ? "" : "s"}`
      : p.scene_count === 1
        ? "1 scene"
        : `${p.scene_count} scenes`;

  return (
    <button
      type="button"
      className={
        "pick-card" +
        (state === "added" ? " is-added" : "") +
        (state === "err" ? " is-err" : "")
      }
      onClick={add}
      disabled={state === "adding" || state === "added"}
      title={
        state === "idle"
          ? `Add ${p.name} to your library`
          : `${p.name} — ${msg || "adding…"}`
      }
      aria-label={`Add ${p.name} to your library`}
    >
      {img && !noImage ? (
        <img
          className="pick-img"
          src={img}
          alt=""
          loading="lazy"
          onError={() => setNoImage(true)}
        />
      ) : (
        <div className="pick-img pick-img-empty" aria-hidden="true">
          {p.name.slice(0, 1)}
        </div>
      )}

      <div className="pick-scrim">
        <div className="pick-name">{p.name}</div>
        <div className="pick-stat">{stat}</div>
      </div>

      {/* The affordance sits over the portrait rather than beside the name:
          the card is one control, and this says which control it is. */}
      <span className="pick-action" aria-hidden="true">
        {state === "adding" ? (
          "…"
        ) : state === "added" ? (
          <CheckIcon size={13} />
        ) : state === "err" ? (
          "!"
        ) : (
          <PlusIcon size={13} />
        )}
      </span>
    </button>
  );
}
