package matcher

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"
)

// The accuracy regression gate.
//
// Matching accuracy was, until now, something measured occasionally by hand
// against a live StashDB and then argued about from memory. That is how the
// headline number came to be quoted as 0.569 for weeks while the real figure
// on production input was 0.953: nothing re-measured it, and nothing failed
// when it drifted.
//
// This runs the shipped verifier over 1,570 recorded corpus entries and fails
// if it gets worse. No network, no API key, ~1s, so it runs on every push.
//
// The floors are set just under the measured values, not at them: the fixture
// is a fixed recording, so the numbers are deterministic, and a floor with no
// slack turns an unrelated refactor into a red build over a rounding change.
// Raise them when a change earns it — that is the ratchet.
const (
	// corpusMinRecall: the expected scene verifies. Measured 0.9529.
	corpusMinRecall = 0.94
	// corpusMinClean: the expected scene verifies AND nothing else does,
	// which is what "identified" actually means. Measured 0.8713.
	corpusMinClean = 0.86
	// corpusMaxFalseVerifies caps the precision side outright. Measured
	// 145. A change that trades recall for a flood of false verifies would
	// otherwise pass both rates above.
	corpusMaxFalseVerifies = 160
)

const corpusFixture = "testdata/corpus-replay.json.gz"

// loadCorpusFixture returns the recorded corpus, or nil when the fixture is
// absent. Absence is a skip rather than a failure: the fixture holds real
// scene ids from a real library, so it is the kind of thing an owner may want
// out of the tree, and that choice must not break the build.
func loadCorpusFixture(t *testing.T) []ReplayEntry {
	t.Helper()
	f, err := os.Open(corpusFixture)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("no corpus fixture; regenerate with matcher-bench --verify --dump")
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var entries []ReplayEntry
	if err := json.NewDecoder(gz).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestCorpusAccuracyDoesNotRegress(t *testing.T) {
	entries := loadCorpusFixture(t)
	if len(entries) < 1000 {
		t.Fatalf("fixture holds %d entries, expected the full corpus", len(entries))
	}
	s := ScoreReplay(DefaultVerifyConfig, entries)
	t.Logf("corpus %d: recall %.4f, clean %.4f, false verifies %d (in %d entries)",
		s.Entries, s.RecallRate(), s.CleanRate(), s.FalseVerifies, s.EntriesWithFalse)

	if s.RecallRate() < corpusMinRecall {
		t.Errorf("recall %.4f below floor %.2f: the verifier now refuses scenes it used to accept",
			s.RecallRate(), corpusMinRecall)
	}
	if s.CleanRate() < corpusMinClean {
		t.Errorf("clean rate %.4f below floor %.2f: fewer entries resolve to exactly one scene",
			s.CleanRate(), corpusMinClean)
	}
	if s.FalseVerifies > corpusMaxFalseVerifies {
		t.Errorf("false verifies %d above cap %d: the verifier accepts more wrong scenes",
			s.FalseVerifies, corpusMaxFalseVerifies)
	}
}

// The fixture must reproduce the live bench. If replaying diverges from the
// run that produced it, every offline experiment is measuring a different
// verifier from the one that ships, and this whole apparatus is decorative.
func TestCorpusFixtureMatchesTheLiveRun(t *testing.T) {
	entries := loadCorpusFixture(t)
	s := ScoreReplay(DefaultVerifyConfig, entries)
	// Recorded from matcher-bench --verify on 2026-07-31 against the same
	// corpus build (commit 627749f, 1,570 confirmed search-grabs).
	const (
		liveEntries       = 1570
		liveRecall        = 1496
		liveFalseVerifies = 145
		liveEntriesFalse  = 128
	)
	if s.Entries != liveEntries || s.Recall != liveRecall ||
		s.FalseVerifies != liveFalseVerifies || s.EntriesWithFalse != liveEntriesFalse {
		t.Errorf("replay diverged from the live run:\n  got  entries=%d recall=%d false=%d entriesWithFalse=%d\n  want entries=%d recall=%d false=%d entriesWithFalse=%d",
			s.Entries, s.Recall, s.FalseVerifies, s.EntriesWithFalse,
			liveEntries, liveRecall, liveFalseVerifies, liveEntriesFalse)
	}
}

// A config that verifies nothing has perfect precision, and one that verifies
// everything has perfect recall. The gate must be able to fail in both
// directions or it only measures one.
func TestCorpusGateCatchesBothFailureDirections(t *testing.T) {
	entries := loadCorpusFixture(t)

	refuseAll := DefaultVerifyConfig
	refuseAll.RankMinTitleOverlap = 1.1
	refuseAll.TitleMinContainment = 1.1
	refuseAll.StrongMatchConf = 1.1
	refuseAll.DateAnchorMinConf = 1.1
	refuseAll.ShortTitleMinConf = 1.1
	if got := ScoreReplay(refuseAll, entries); got.RecallRate() >= corpusMinRecall {
		t.Errorf("a verifier that refuses everything scored recall %.4f, above the floor",
			got.RecallRate())
	}

	acceptAll := DefaultVerifyConfig
	acceptAll.RankMinTitleOverlap = 0
	acceptAll.TitleMinContainment = 0
	acceptAll.TitleMinConf = 0
	acceptAll.StrongMatchConf = 0
	if got := ScoreReplay(acceptAll, entries); got.FalseVerifies <= corpusMaxFalseVerifies {
		t.Errorf("a verifier that accepts everything produced %d false verifies, under the cap",
			got.FalseVerifies)
	}
}
