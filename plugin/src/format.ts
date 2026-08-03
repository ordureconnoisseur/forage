// humanSize formats a byte count as a compact "1.5GB"-style string.
// `zero` is what to return for a falsy/zero input — "" to render nothing
// (most call sites) or "?" where an explicit unknown reads better (e.g.
// a pack whose size the indexer didn't report).
export function humanSize(b: number, zero = ""): string {
  if (!b) return zero;
  // units[0] is the bare byte case — appending the trailing "B" to it
  // rendered sub-1KB sizes as "512.0BB".
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = b;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return (i === 0 ? String(v) : v.toFixed(1)) + units[i];
}

// filterGlyph maps a content filter's gender set to its symbol, so the
// toolbar chip can be a compact icon instead of a text label. The
// trailing U+FE0E variation selector forces the monochrome text glyph
// (iOS otherwise renders these as coloured emoji). Mixed sets we have
// no single symbol for return null; the chip falls back to its name.
export function filterGlyph(genders?: string[]): string | null {
  if (!genders || genders.length === 0) return null;
  const trans = (g: string) =>
    g.startsWith("TRANSGENDER") || g === "NON_BINARY";
  if (genders.every(trans)) return "⚧︎"; // ⚧
  if (genders.every((g) => g === "FEMALE")) return "♀︎"; // ♀
  if (genders.every((g) => g === "MALE")) return "♂︎"; // ♂
  if (genders.every((g) => g === "INTERSEX")) return "⚥︎"; // ⚥
  return null;
}

/**
 * The folder forage files into when it cannot attribute a scene to anyone.
 *
 * "Unfiled" is the name the product uses everywhere else (the /unfiled route,
 * the Unfiled view), so it is what a fresh library gets. "Unsorted" is what
 * older libraries call the same bin, and both are recognised forever: the
 * daemon keeps writing whichever one a library already has, so a rename never
 * splits one bin into two.
 */
export const UNFILED_FOLDERS = ["Unfiled", "Unsorted"] as const;

/** Whether a performer/folder name is really the fallback bin, not a person. */
export function isUnfiledFolder(name: string | null | undefined): boolean {
  if (!name) return false;
  const n = name.trim().toLowerCase();
  return UNFILED_FOLDERS.some((f) => f.toLowerCase() === n);
}
