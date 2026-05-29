// humanSize formats a byte count as a compact "1.5GB"-style string.
// `zero` is what to return for a falsy/zero input — "" to render nothing
// (most call sites) or "?" where an explicit unknown reads better (e.g.
// a pack whose size the indexer didn't report).
export function humanSize(b: number, zero = ""): string {
  if (!b) return zero;
  const units = ["B", "K", "M", "G", "T"];
  let i = 0;
  let v = b;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return v.toFixed(1) + units[i] + "B";
}
