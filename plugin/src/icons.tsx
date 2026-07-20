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
