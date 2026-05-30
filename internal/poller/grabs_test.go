package poller

import "testing"

// TestPackScanCoverageOK guards the floor that stops a pack confirming
// against a half-scanned directory after a restart re-seeds the
// in-memory settle window at a partial count.
func TestPackScanCoverageOK(t *testing.T) {
	cases := []struct {
		name           string
		found, expected int
		want           bool
	}{
		{"unknown expected count → no floor", 1, 0, true},
		{"negative expected treated as unknown", 5, -1, true},
		{"fully indexed", 126, 126, true},
		{"exactly at floor (80%)", 80, 100, true},
		{"just over floor", 81, 100, true},
		{"just under floor", 79, 100, false},
		{"restart mid-scan: 400 of 1314", 400, 1314, false},
		{"nearly complete 1300 of 1314", 1300, 1314, true},
		{"over-count (title overstated, all real files in)", 130, 126, true},
		{"zero found, real expected", 0, 50, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := packScanCoverageOK(c.found, c.expected); got != c.want {
				t.Errorf("packScanCoverageOK(%d, %d) = %v, want %v",
					c.found, c.expected, got, c.want)
			}
		})
	}
}
