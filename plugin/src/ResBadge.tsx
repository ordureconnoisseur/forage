// Resolution badge shared by the collection triage list and the
// per-scene releases page. Pulls a quality label out of a release
// title; renders nothing when no resolution token is present (e.g. a
// bare "SiteRip"). 4K pops gold, 1080p blue, lower tiers muted (see
// .res-badge styles in global.css).
export function resolution(
  title: string,
): { label: string; cls: string } | null {
  // Underscores fold to spaces: `\b` (the boundary in each token below) is a
  // regex word boundary and `_` is a word char, so a release ending
  // "…2022._1080p" has no boundary before the digit and `\b1080p\b` misses it.
  // Dots/dashes already separate cleanly; underscore was the lone gap.
  const t = title.toLowerCase().replace(/_/g, " ");
  // VR "NK" labels (5K–8K) — VR rips are labelled by a "K" width (Oculus 7K)
  // and outrank flat 4K. Shown with the 4K gold styling (all high-res).
  const k = t.match(/\b([5-8])k\b/);
  if (k) return { label: k[1] + "K", cls: "res-4k" };
  // 4K is named both by height (2160p) and width (3840p) in the wild.
  if (/\b(2160p?|3840p?|4k|uhd)\b/.test(t)) return { label: "4K", cls: "res-4k" };
  // VR pixel-height labels (e.g. VR180 3600p). 2160/3840 are flat-4K labels
  // caught above; a tall 4-digit height maps to the matching VR K tier, while
  // 1081–2159 (e.g. an Oculus Go 1920p cut) shows its own height.
  const ph = t.match(/\b(\d{4})p\b/);
  if (ph) {
    const n = +ph[1];
    if (n >= 4000) return { label: "8K", cls: "res-4k" };
    if (n >= 3200) return { label: "7K", cls: "res-4k" };
    if (n >= 2700) return { label: "6K", cls: "res-4k" };
    if (n >= 2160) return { label: "5K", cls: "res-4k" };
    if (n > 1080) return { label: ph[1] + "p", cls: "res-1080" };
  }
  // FHD / FHDC (JAV/sukebei "Full HD") are 1080p.
  if (/\b(1080p?|fhdc?)\b/.test(t)) return { label: "1080p", cls: "res-1080" };
  if (/\b720p?\b/.test(t)) return { label: "720p", cls: "res-720" };
  if (/\b480p?\b/.test(t)) return { label: "480p", cls: "res-480" };
  return null;
}

export function ResBadge({ title }: { title: string }) {
  const res = resolution(title);
  if (!res) return null;
  return <span className={"res-badge " + res.cls}>{res.label}</span>;
}
