# UI refinement brief

A design pass over forage's interface. This is a brief, not a spec: it says
what is wrong and what "better" has to mean, and leaves the solutions open.

## What forage is

A daemon that finds, downloads and files adult scenes into a Stash library.
One user, their own server, used from a phone as often as a desktop. It is
not a consumer product with an onboarding funnel; it is a tool someone
operates repeatedly and knows well. That matters: **familiarity is worth more
than discoverability here.** Clever affordances that need learning once are
fine. Ambiguity that has to be re-read every time is not.

Stack: React + TypeScript, Vite, one hand-written `global.css` (~5,500 lines,
no framework, no CSS-in-JS). Ships as a single self-contained `index.html`
embedded in the Go binary. Dark theme only, green accent.

## The problem, specifically

The trigger was the Discover controls, and they are the clearest case.

`DiscoverList.tsx` renders one flex container holding, as flat siblings: a
search input, a stash-box `<select>`, a days `<select>`, a "Favourites only"
checkbox, zero or more icon chips, a `382 / 382` count, and a Select button.
Seven controls of six different shapes, no grouping, no hierarchy. On a phone
that wraps into four ragged rows and consumes most of a screen before any
content appears.

Concrete faults visible in that cluster:

- **The two selects have arbitrary different widths** because each is sized by
  its content. Nothing aligns to anything.
- **The gender-filter chip is an unlabelled circular glyph.** Its meaning is
  carried entirely by a `title` attribute, which does not exist on touch.
- **`382 / 382` is unlabelled.** It is filtered-over-total, but nothing says
  so, and it sits between two interactive controls as though it were one.
- **"Select" is a bare pill** that enters multi-select mode. Nothing
  distinguishes it from a filter control beside it.
- **Four distinct control idioms** (input, select, checkbox, chip, plain text,
  button) in one bar, each with its own height, radius and border treatment.

The scene cards have their own version of the same problem:

- **Badges conflate state and action.** Green "Ready", amber "Watching", grey
  "Watch" sit in the same position on the thumbnail. Two are states, one is a
  verb. Colour is doing work that shape and wording should share.
- **Titles truncate mid-word** with an ellipsis, so "Stepsister's Seduction:
  JoJo…" tells you less than the space allows.
- **The date · studio line wraps unpredictably** ("2026-08-06 · SOD Create"
  becomes two lines), so cards in one row have different heights.
- **The performer chip sometimes carries a `+` and sometimes does not**, with
  no explanation of the difference.

## Constraints

These are not preferences; breaking them breaks the product.

1. **`days` is an existing user setting** (presets 7/30/60/90, persisted, and
   capped at 90 server-side). It is the control for how much you see. A
   redesign must keep it as such and must not introduce a second control that
   reads like it. `RENDER_CAP` bounds what is *mounted* and is deliberately not
   a setting.
2. **Mobile is a first-class surface**, not a fallback. The screenshot that
   prompted this is an iPhone. Anything that only works at desktop widths is
   not a solution.
3. **Some controls conditionally disappear.** The days select, the favourites
   checkbox and the filter chips are all hidden when a secondary stash-box is
   selected, because they have no meaning against it. Any layout has to survive
   controls vanishing without collapsing into something ugly.
4. **The deployment filters are dynamic.** Their names and glyphs come from
   config, so the chip row can be empty, or hold several, and the labels are
   not known at design time.
5. **No new dependencies.** One CSS file, no framework, no icon package. The
   bundle is embedded in the binary and served under a strict CSP with no
   external origins.
6. **Dark theme only.** There is no light mode to design for.

## What "better" has to mean

Not "prettier". The measures are:

- **Fewer rows before content.** The controls should occupy noticeably less
  vertical space on a 390pt-wide screen than they do now.
- **One visual idiom per job.** Filters look like filters, actions look like
  actions, and readouts do not look interactive.
- **Every control readable without a tooltip.** A glyph may stay a glyph if it
  is unambiguous in context, but `title` must not be the only explanation.
- **Cards in a row have equal height** and titles use the space they have.
- **State and action are distinguishable at a glance** on a thumbnail, without
  relying on colour alone.

## Scope

In scope, in priority order:

1. The Discover control cluster (the trigger).
2. The scene card: badge, title, meta line, performer chip.
3. The same control pattern where it recurs — Missing Scenes and Grabs share
   both the filter-bar shape and the card, so a fix should generalise rather
   than fork.

Out of scope for this pass: the top navigation, Settings, the setup wizard,
and anything requiring backend changes.

## Non-goals

- A restyle for its own sake. The dark/green identity stays.
- Animation as decoration. The one animation added recently exists because a
  loading thumbnail was indistinguishable from a broken one.
- Density for its own sake: this is browsed on a phone, so touch targets stay
  comfortable even if that costs pixels.

## Where to look

- `plugin/src/views/DiscoverList.tsx` — the control cluster (~line 360) and
  `DiscoverCard` (~line 748).
- `plugin/src/views/MissingScenes.tsx`, `plugin/src/views/GrabsList.tsx` — the
  same patterns.
- `plugin/src/styles/global.css` — `.scene-thumb`, `.scene-grid`,
  `.grab-chip`, `.check`, `.count`.

Verify against the running UI rather than the source: forage is reachable over
the tailnet, and there is a CDP recipe in the repo's notes for driving it
headless. Note that `imageBase()` returns null on `localhost`, so images do not
load when the app is reached by that hostname — use another name or the
thumbnails will all appear broken and send you chasing the wrong problem.
