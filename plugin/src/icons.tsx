// Shared line icons in the app's stroke language (24 viewBox,
// currentColor, round caps), same as NavIcons and WatchControl's
// bookmark. No emoji: glyph rendering varies across platforms.

function iconProps(size: number) {
  return {
    viewBox: "0 0 24 24",
    width: size,
    height: size,
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 2,
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    "aria-hidden": true,
  };
}

// Bolt outline (SVG Repo, CC0): the auto-grab toggle.
export function BoltIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <path d="M12.9996 3L5.06859 12.6934C4.72703 13.1109 4.55625 13.3196 4.55471 13.4956C4.55336 13.6486 4.62218 13.7939 4.74148 13.8897C4.87867 14 5.14837 14 5.68776 14H11.9996L10.9996 21L18.9305 11.3066C19.2721 10.8891 19.4429 10.6804 19.4444 10.5044C19.4458 10.3514 19.377 10.2061 19.2577 10.1103C19.1205 10 18.8508 10 18.3114 10H11.9996L12.9996 3Z" />
    </svg>
  );
}

// Notification bell: subscription activity badges.
export function BellIcon({ size = 10 }: { size?: number }) {
  return (
    <svg {...iconProps(size)} strokeWidth={2.4}>
      <path d="M18 15.5V11a6 6 0 1 0-12 0v4.5L4.5 18h15L18 15.5z" />
      <path d="M10 21a2.2 2.2 0 0 0 4 0" />
    </svg>
  );
}

// Gender symbols, in the same stroke language as the rest of this file.
//
// These replace the unicode glyphs the content filters used to render
// (⚧︎ ♀︎ ♂︎ ⚥︎). Those depend on a font actually carrying the codepoint AND on
// the variation selector being honoured, so the same chip looked different on
// every platform and thin-to-invisible on some — which is the exact reason
// this file opens with "No emoji: glyph rendering varies across platforms".
//
// The Venus and Mars geometry is taken from binge's GenderIcon so the two
// apps draw the same symbol; the trans and intersex forms are built in the
// same idiom, since binge only needed the per-gender variants.
export function GenderIcon({
  kind,
  size = 13,
}: {
  kind: "trans" | "female" | "male" | "intersex";
  size?: number;
}) {
  return (
    <svg {...iconProps(size)} strokeWidth={1.9}>
      {kind === "female" && (
        <g>
          <circle cx="12" cy="9" r="4.5" />
          <path d="M12 13.5 v7" />
          <path d="M9 17.5 h6" />
        </g>
      )}
      {kind === "male" && (
        <g>
          <circle cx="10" cy="14" r="4.5" />
          <path d="M13.2 10.8 L20 4" />
          <path d="M14 4 H20 V10" />
        </g>
      )}
      {kind === "intersex" && (
        // Venus below, Mars above-right: the two arms of ⚥ on one ring.
        <g>
          <circle cx="12" cy="12" r="4" />
          <path d="M12 16 v5" />
          <path d="M9.5 18.7 h5" />
          <path d="M14.9 9.1 L19.5 4.5" />
          <path d="M15.5 4.5 H19.5 V8.5" />
        </g>
      )}
      {kind === "trans" && (
        // ⚧: the same two arms, plus the third to the upper LEFT with a bar
        // across its tip — the stroke that distinguishes the trans symbol
        // from a plain intersex one.
        <g>
          <circle cx="12" cy="12" r="4" />
          <path d="M12 16 v5" />
          <path d="M9.5 18.7 h5" />
          <path d="M14.9 9.1 L19.5 4.5" />
          <path d="M15.5 4.5 H19.5 V8.5" />
          <path d="M9.1 9.1 L4.9 4.9" />
          <path d="M3.7 6.6 L6.6 3.7" />
        </g>
      )}
    </svg>
  );
}

// Heart, for the "Favourites" scope toggle.
//
// The path is binge's (HeartBurst's HEART_PATH) so the two apps draw the same
// heart rather than two that nearly match. It replaces a "♥" character, which
// had the same font-dependence as the gender glyphs did.
//
// Outline when idle, filled when engaged — the same grammar BookmarkGlyph uses
// for watch state, so a filled shape means "on" consistently across the app.
const HEART_PATH =
  "M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 " +
  "3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78" +
  "-3.4 6.86-8.55 11.54L12 21.35z";

export function HeartIcon({
  size = 13,
  filled = false,
}: {
  size?: number;
  filled?: boolean;
}) {
  return (
    <svg
      {...iconProps(size)}
      strokeWidth={1.7}
      fill={filled ? "currentColor" : "none"}
    >
      <path d={HEART_PATH} />
    </svg>
  );
}

// Plus / check / alert for the "add this performer" pill.
//
// The plus path is the one the other Stash plugins use (M12 5v14M5 12h14) so
// the shape is shared rather than re-invented. It replaces a "+" character,
// which sat at whatever size and baseline the font felt like and gave no hint
// that it was a control.
export function PlusIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)} strokeWidth={2.2}>
      <path d="M12 5v14M5 12h14" />
    </svg>
  );
}

export function CheckIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)} strokeWidth={2.2}>
      <path d="M4 12.5 L9.5 18 L20 6.5" />
    </svg>
  );
}

// A right chevron, for "there is more this way". Same stroke language as the
// rest; the carousel's own arrows are separate glyphs baked into that
// component.
export function ChevronIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)} strokeWidth={2.2}>
      <path d="M9 5l7 7-7 7" />
    </svg>
  );
}

// An X, for dismissing. Same stroke language and weight as the plus it sits
// opposite, so the pair reads as a pair.
export function CloseIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)} strokeWidth={2.2}>
      <path d="M6 6l12 12M18 6L6 18" />
    </svg>
  );
}

// ── the glyphs that were text ──────────────────────────────────────
//
// The file opens with a rule ("No emoji: glyph rendering varies across
// platforms") and the app then shipped 100 raw characters doing icon work:
// arrows, ticks, crosses, stars, a hourglass, a skull. They render at
// whatever weight and baseline each platform's fallback font decides, in a
// different face from every real icon beside them, and several are emoji on
// some systems and line art on others. These are the replacements, in the
// same 24-box stroke language as the rest.

export function ArrowRightIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <path d="M4 12h15M13 6l6 6-6 6" />
    </svg>
  );
}

export function ArrowDownIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <path d="M12 4v15M6 13l6 6 6-6" />
    </svg>
  );
}

export function ArrowUpIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <path d="M12 20V5M6 11l6-6 6 6" />
    </svg>
  );
}

// Out of forage and into someone else's site: the box you leave, the arrow
// leaving it.
export function ExternalIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <path d="M14 4h6v6M20 4l-8.5 8.5" />
      <path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" />
    </svg>
  );
}

export function RefreshIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <path d="M20 11a8 8 0 1 0-.6 4" />
      <path d="M20 4v7h-7" />
    </svg>
  );
}

// Filled when it is a favourite, outline when it is not. Same path either
// way, so the two states cannot drift apart in shape.
export function StarIcon({
  size = 12,
  filled = false,
}: {
  size?: number;
  filled?: boolean;
}) {
  return (
    <svg {...iconProps(size)} fill={filled ? "currentColor" : "none"}>
      <path d="M12 3.5l2.6 5.3 5.9.85-4.25 4.15 1 5.85L12 16.9l-5.25 2.75 1-5.85L3.5 9.65l5.9-.85z" />
    </svg>
  );
}

export function WarnIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <path d="M12 4.5 21 20H3z" />
      <path d="M12 10v4.5" />
      <path d="M12 17.4v.1" />
    </svg>
  );
}

// Refused, not merely failed: a circle with a bar through it. Used where the
// app was reaching for a "no entry" emoji.
export function BlockedIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M6 18 18 6" />
    </svg>
  );
}

// Dead: the app used a skull. A circle with a cross in it says the same
// thing in the same language as everything around it, and does not become a
// colour emoji on a Mac.
export function DeadIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M9 9l6 6M15 9l-6 6" />
    </svg>
  );
}

// Waiting on something out of our hands.
export function ClockIcon({ size = 12 }: { size?: number }) {
  return (
    <svg {...iconProps(size)}>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M12 7v5.2l3.2 2" />
    </svg>
  );
}
