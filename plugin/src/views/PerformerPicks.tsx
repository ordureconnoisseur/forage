import { useEffect, useRef, useState } from "react";
import {
  addPerformerFromStashDB,
  dismissPerformerPick,
  fetchDiscoverPerformers,
  stashdbImageURL,
  type DiscoverPerformerPick,
  type DiscoverPerformersResponse,
} from "../api";
import { CheckIcon, CloseIcon, PlusIcon } from "../icons";

// Who to add next, between the trending scenes and your own performers' new
// releases. All three sections on Discover answer "what should I look at",
// ordered by how far they sit from the library you already have.
//
// Three lenses, one strip. They could have been three strips, but they answer
// one question from three angles and stacking them would push the feed three
// screens down to make that point.
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
  // Hidden locally the moment they are dismissed. The server applies the same
  // set when it next serves the strip; this is so the card leaves under the
  // finger rather than on the next page load.
  const [gone, setGone] = useState<Set<string>>(new Set());

  useEffect(() => {
    let cancelled = false;
    fetchDiscoverPerformers()
      .then((d) => !cancelled && setData(d))
      .catch(() => !cancelled && setFailed(true));
    return () => {
      cancelled = true;
    };
  }, []);

  // Nothing at all rather than an empty frame. This is a suggestion strip
  // sitting between two things the user actually asked for, and a broken
  // looking one is worse than none. Same reason the subscriptions row hides
  // itself.
  if (failed) return null;

  const picks = (data ? data[lens] : []).filter((p) => !gone.has(p.stashdb_id));
  const loading = !data;
  if (data && !data.trending.length && !data.debut.length && !data.active.length) {
    return null;
  }

  return (
    <section className="picks-section">
      <div className="picks-head">
        <h3 className="section-header">Discover performers</h3>
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

      {/* The same shell the scene carousel uses on a phone: .carousel-row with
          snap scrolling, the same gap, the same card chrome. It does not page
          with chevrons the way the scene strip does at desktop width, because
          twenty-four narrow cards would be eight pages of clicking where one
          swipe does; everything else is deliberately identical, so the two
          strips on this page read as the same component. */}
      <div className="trending-carousel scroll picks-carousel">
        <div className="carousel-row">
        {loading
          ? Array.from({ length: 8 }, (_, i) => (
              <div
                className="trending-card pick-card is-skeleton"
                key={i}
                aria-hidden="true"
              />
            ))
          : picks.map((p) => (
              <PickCard
                key={p.stashdb_id}
                p={p}
                lens={lens}
                onDismiss={() =>
                  setGone((prev) => new Set(prev).add(p.stashdb_id))
                }
              />
            ))}
        </div>
      </div>
    </section>
  );
}

function PickCard({
  p,
  lens,
  onDismiss,
}: {
  p: DiscoverPerformerPick;
  lens: Lens;
  onDismiss: () => void;
}) {
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
    <div
      className={
        "trending-card pick-card" +
        (state === "added" ? " is-added" : "") +
        (state === "err" ? " is-err" : "")
      }
    >
      <div className="scene-thumb pick-thumb">
        {img && !noImage ? (
          <img src={img} alt="" loading="lazy" onError={() => setNoImage(true)} />
        ) : (
          <div className="pick-thumb-empty" aria-hidden="true">
            {p.name.slice(0, 1)}
          </div>
        )}
        {/* The same overlay pill the scene cards carry for Watch, in the same
            corner at the same size. It replaces a bare "+" in a circle, which
            said nothing about what it did and competed with the dismiss for
            attention in the opposite corner. */}
        <button
          type="button"
          className={"watch-chip pick-add" + (state === "added" ? " is-added" : "")}
          onClick={add}
          disabled={state === "adding" || state === "added"}
          title={
            state === "idle"
              ? `Add ${p.name} to your library`
              : `${p.name}: ${msg || "adding…"}`
          }
          aria-label={`Add ${p.name} to your library`}
        >
          {state === "adding" ? (
            "…"
          ) : state === "added" ? (
            <>
              <CheckIcon size={11} /> Added
            </>
          ) : state === "err" ? (
            "!"
          ) : (
            <>
              <PlusIcon size={11} /> Add
            </>
          )}
        </button>
        {/* Not interested. Deliberately the quieter of the two: it is the
            rarer action, and a strip that gets swiped past should not carry
            two equally loud targets. Revealed on hover where there is a
            pointer, always present on touch, which has none. */}
        <button
          type="button"
          className="pick-dismiss"
          aria-label={`Stop suggesting ${p.name}`}
          title={`Stop suggesting ${p.name}`}
          onClick={() => {
            void dismissPerformerPick(p.stashdb_id).catch(() => {});
            onDismiss();
          }}
        >
          <CloseIcon size={10} />
        </button>
      </div>
      <div className="trending-card-body">
        <div className="trending-card-title pick-name">{p.name}</div>
        <div className="trending-card-meta">{stat}</div>
      </div>
    </div>
  );
}
