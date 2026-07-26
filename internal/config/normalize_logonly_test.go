package config

import "testing"

// TestNormalizePackKeep pins the accepted PackDedupKeep values — most
// importantly that "log-only" (the dry-run mode) survives normalisation and
// that anything unrecognised falls back to the safe default rather than an
// armed mode.
func TestNormalizePackKeep(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{" LOG-ONLY ", "log-only"},
		{"log-only", "log-only"},
		{"pack", "pack"},
		{"review", "review"},
		{"both", "both"},
		{"junk", "existing"},
		{"", "existing"},
	} {
		if got := normalizePackKeep(tc.in); got != tc.want {
			t.Errorf("normalizePackKeep(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
