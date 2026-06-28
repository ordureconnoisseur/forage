package api

import "testing"

// TestParsePublishUnix pins the RSS watermark date parse: RFC3339 -> unix, and
// 0 for unparseable/empty (those releases are skipped — we can't watermark what
// we can't date).
func TestParsePublishUnix(t *testing.T) {
	if got := parsePublishUnix("2026-06-28T00:24:31Z"); got != 1782606271 {
		t.Errorf("parsePublishUnix(RFC3339) = %d, want 1782606271", got)
	}
	if got := parsePublishUnix(""); got != 0 {
		t.Errorf("parsePublishUnix(empty) = %d, want 0", got)
	}
	if got := parsePublishUnix("not a date"); got != 0 {
		t.Errorf("parsePublishUnix(garbage) = %d, want 0", got)
	}
}
