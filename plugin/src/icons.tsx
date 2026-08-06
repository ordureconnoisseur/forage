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
