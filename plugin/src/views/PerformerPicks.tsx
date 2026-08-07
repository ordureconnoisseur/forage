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
                className="performer-card pick-card is-skeleton"
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

  // Not printed on the card any more: a 132px portrait wants a name on it,
  // not two lines of type. It still says why this person is in the strip, so
  // it moves to the tooltip rather than being thrown away. On trending it is
  // the ranking signal itself; elsewhere the ORDER carries the recency
  // (StashDB exposes no debut or last-scene DATE to print), so the scene
  // count is what is left worth saying.
  const stat =
    lens === "trending" && p.trending_scenes
      ? `on ${p.trending_scenes} trending scene${p.trending_scenes === 1 ? "" : "s"}`
      : p.scene_count === 1
        ? "1 scene"
        : `${p.scene_count} scenes`;

  // forage already has a performer card: portrait filling the frame, name and
  // stat on a scrim across the bottom, used for every performer in the
  // library. This is a performer, so it is that card. Borrowing the SCENE
  // card instead put the picture in a small box above a body block, which is
  // right for a 16:9 still and wrong for a face.
  return (
    <button
      type="button"
      className={
        "performer-card pick-card" +
        (state === "added" ? " is-added" : "") +
        (state === "err" ? " is-err" : "")
      }
      onClick={add}
      disabled={state === "adding" || state === "added"}
      title={
        state === "idle"
          ? `${p.name}${p.age ? `, ${p.age}` : ""}, ${stat}. Add to your library`
          : `${p.name}: ${msg || "adding…"}`
      }
      aria-label={`Add ${p.name} to your library`}
    >
      {img && !noImage ? (
        <img
          className="perf-img"
          src={img}
          alt=""
          loading="lazy"
          onError={() => setNoImage(true)}
        />
      ) : (
        <div className="perf-img perf-img-empty" aria-hidden="true">
          {p.name.slice(0, 1)}
        </div>
      )}

      {/* The two controls are not peers, so they do not look like peers.
          Adding is why this strip exists; dismissing is housekeeping. Two
          matched circles in opposite corners asserted the opposite, and on a
          132px card two 22px discs are most of the frame.

          So they live on different planes. The add is IN the scrim, leading
          the name, which is the grammar forage already uses for a performer
          it does not have: "+ Jane Doe" on a Discover chip means add Jane
          Doe. Verb, then object. The dismiss stays on the image, alone in
          one corner, with no disc behind it at all. */}
      <div className="perf-scrim">
        <span className="pick-add" aria-hidden="true">
          {state === "adding" ? (
            <span className="pick-spinner" />
          ) : state === "added" ? (
            <CheckIcon size={11} />
          ) : state === "err" ? (
            "!"
          ) : (
            <PlusIcon size={11} />
          )}
        </span>
        <span className="perf-name">{p.name}</span>
        {/* Just the number, pinned to the end of the row so a name that wraps
            does not drag it down. Absent when StashDB has no birthdate. */}
        {p.age ? (
          <span className="pick-age" title={`${p.age} years old`}>
            {p.age}
          </span>
        ) : null}
      </div>

      <span
        className="pick-dismiss"
        role="button"
        tabIndex={0}
        aria-label={`Stop suggesting ${p.name}`}
        title={`Stop suggesting ${p.name}`}
        onClick={(e) => {
          // The card underneath adds. Without this, dismissing would add.
          e.preventDefault();
          e.stopPropagation();
          void dismissPerformerPick(p.stashdb_id).catch(() => {});
          onDismiss();
        }}
        onKeyDown={(e) => {
          if (e.key !== "Enter" && e.key !== " ") return;
          e.preventDefault();
          e.stopPropagation();
          void dismissPerformerPick(p.stashdb_id).catch(() => {});
          onDismiss();
        }}
      >
        <CloseIcon size={11} />
      </span>
    </button>
  );
}
