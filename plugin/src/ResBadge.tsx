// Resolution badge shared by the collection triage list and the
// per-scene releases page. Pulls a quality label out of a release
// title; renders nothing when no resolution token is present (e.g. a
// bare "SiteRip"). 4K pops gold, 1080p blue, lower tiers muted (see
// .res-badge styles in global.css).
export function resolution(
  title: string,
): { label: string; cls: string } | null {
  const t = title.toLowerCase();
  // 4K is named both by height (2160p) and width (3840p) in the wild.
  if (/\b(2160p?|3840p?|4k|uhd)\b/.test(t)) return { label: "4K", cls: "res-4k" };
  if (/\b1080p?\b/.test(t)) return { label: "1080p", cls: "res-1080" };
  if (/\b720p?\b/.test(t)) return { label: "720p", cls: "res-720" };
  if (/\b480p?\b/.test(t)) return { label: "480p", cls: "res-480" };
  return null;
}

export function ResBadge({ title }: { title: string }) {
  const res = resolution(title);
  if (!res) return null;
  return <span className={"res-badge " + res.cls}>{res.label}</span>;
}
