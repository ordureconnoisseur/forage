// A VR "K" label names a WIDTH, so it needs a height to sort by. The exact
// numbers matter less than the order: these are the usual stitched heights,
// and all of them outrank flat 4K.
const K_HEIGHT: Record<string, number> = {
  "5": 2880,
  "6": 3600,
  "7": 3840,
  "8": 4320,
};

// Resolution badge shared by the collection triage list and the
// per-scene releases page. Pulls a quality label out of a release
// title; renders nothing when no resolution token is present (e.g. a
// bare "SiteRip"). 4K pops gold, 1080p blue, lower tiers muted (see
// .res-badge styles in global.css).
// height is what the label is worth when SORTING. It rides along with the
// label so a caller cannot rank a title by rules that disagree with what the
// badge beside it says: SceneReleases had its own resolutionRank() that did
// not fold underscores and knew nothing about VR, so a release could show a
// gold 5K badge and sort as if it had no resolution at all.
export function resolution(
  title: string,
): { label: string; cls: string; height: number } | null {
  // Underscores fold to spaces: `\b` (the boundary in each token below) is a
  // regex word boundary and `_` is a word char, so a release ending
  // "…2022._1080p" has no boundary before the digit and `\b1080p\b` misses it.
  // Dots/dashes already separate cleanly; underscore was the lone gap.
  const t = title.toLowerCase().replace(/_/g, " ");
  // VR "NK" labels (5K–8K) — VR rips are labelled by a "K" width (Oculus 7K)
  // and outrank flat 4K. Shown with the 4K gold styling (all high-res).
  const k = t.match(/\b([5-8])k\b/);
  if (k) return { label: k[1] + "K", cls: "res-4k", height: K_HEIGHT[k[1]] };
  // 4K is named both by height (2160p) and width (3840p) in the wild.
  if (/\b(2160p?|3840p?|4k|uhd)\b/.test(t)) return { label: "4K", cls: "res-4k", height: 2160 };
  // VR pixel-height labels (e.g. VR180 3600p). 2160/3840 are flat-4K labels
  // caught above; a tall 4-digit height maps to the matching VR K tier, while
  // 1081–2159 (e.g. an Oculus Go 1920p cut) shows its own height.
  const ph = t.match(/\b(\d{4})p\b/);
  if (ph) {
    const n = +ph[1];
    if (n >= 4000) return { label: "8K", cls: "res-4k", height: K_HEIGHT["8"] };
    if (n >= 3200) return { label: "7K", cls: "res-4k", height: K_HEIGHT["7"] };
    if (n >= 2700) return { label: "6K", cls: "res-4k", height: K_HEIGHT["6"] };
    if (n >= 2160) return { label: "5K", cls: "res-4k", height: K_HEIGHT["5"] };
    if (n > 1080) return { label: ph[1] + "p", cls: "res-1080", height: n };
  }
  // FHD / FHDC (JAV/sukebei "Full HD") are 1080p.
  if (/\b(1080p?|fhdc?)\b/.test(t)) return { label: "1080p", cls: "res-1080", height: 1080 };
  if (/\b720p?\b/.test(t)) return { label: "720p", cls: "res-720", height: 720 };
  if (/\b480p?\b/.test(t)) return { label: "480p", cls: "res-480", height: 480 };
  return null;
}

// codec pulls an SD-era codec marker (XviD/DivX) out of a release title. These
// are NOT resolutions — they're MPEG-4 ASP codecs — but in practice they mark
// an old, low-quality SD rip (HD XviD essentially doesn't exist), and such
// titles often carry no resolution token at all. Surfaced as its own muted
// badge so an otherwise-blank release reads as "old SD codec", not a mystery.
export function codec(title: string): string | null {
  const m = title.toLowerCase().match(/\b(xvid|divx)\b/);
  if (!m) return null;
  return m[1] === "divx" ? "DivX" : "XviD";
}

export function ResBadge({ title }: { title: string }) {
  const res = resolution(title);
  const cod = codec(title);
  if (!res && !cod) return null;
  return (
    <>
      {res && <span className={"res-badge " + res.cls}>{res.label}</span>}
      {cod && (
        <span
          className="res-badge res-codec"
          title="Old SD-era codec (XviD/DivX), low quality"
        >
          {cod}
        </span>
      )}
    </>
  );
}
