package matcher

import (
	"errors"
	"testing"
)

// TestMatchOutcomeErr locks the graceful-degradation rule: a partial source
// failure (some candidates found, some queries errored) must NOT fail the
// match, because every caller discards candidates on a non-nil error. A
// failure is fatal only when no candidates survived at all.
func TestMatchOutcomeErr(t *testing.T) {
	boom := errors.New("track A: stashdb 502")
	cases := []struct {
		name       string
		candidates int
		errs       []error
		wantErr    bool
	}{
		{"partial failure with candidates degrades", 3, []error{boom}, false},
		{"total failure with no candidates errors", 0, []error{boom}, true},
		{"clean no-match is not an error", 0, nil, false},
		{"clean match is not an error", 5, nil, false},
		{"many candidates, many errors still degrades", 10, []error{boom, boom}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := matchOutcomeErr(tc.candidates, tc.errs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("matchOutcomeErr(%d, %d errs) = %v; wantErr=%v",
					tc.candidates, len(tc.errs), err, tc.wantErr)
			}
		})
	}
}
