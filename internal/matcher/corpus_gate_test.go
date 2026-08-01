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
	// which is what "identified" actually means. Measured 0.8803, up from
	// 0.8713 when the behind-the-scenes guard landed. Ratcheted with it:
	// a floor left at the old value would let that improvement be undone
	// silently, which is the entire point of having a floor.
	corpusMinClean = 0.87
	// corpusMaxFalseVerifies caps the precision side outright. Measured
	// 127, down from 145. A change that trades recall for a flood of false
	// verifies would otherwise pass both rates above.
	corpusMaxFalseVerifies = 140
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

// recordedConfig is the verifier as it stood when the fixture was recorded.
//
// Pinned as a literal rather than referring to DefaultVerifyConfig, which was
// the first version's mistake: it made the faithfulness check below fail
// whenever the verifier legitimately improved, so it was really asserting
// "nobody has changed anything" while claiming to assert "the replay is
// faithful". Those are different questions and only the second is useful.
var recordedConfig = VerifyConfig{
	RankMinTitleOverlap:         0.15,
	TitleMinTokens:              4,
	TitleMinContainment:         0.80,
	TitleMinConf:                0.30,
	StrongTitleOverlap:          0.40,
	ShortTitleMaxTokens:         2,
	ShortTitleMinConf:           0.50,
	StrongMatchConf:             0.70,
	StrongMatchRivalTitleMargin: 0.15,
	DateAnchorMinConf:           0.65,
	StrongMatchNeedsDate:        false,
	ShortTitleNeedsContainment:  true,
	RefuseBehindTheScenes:       false,
	TopMargin:                   0,
}

// The fixture must reproduce the live bench, run under the config that
// produced it. If replaying diverges, every offline experiment is measuring a
// different verifier from the one that ran, and this whole apparatus is
// decorative.
func TestCorpusFixtureMatchesTheLiveRun(t *testing.T) {
	entries := loadCorpusFixture(t)
	s := ScoreReplay(recordedConfig, entries)
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

// And the shipped verifier must be at least as good as the recording. This is
// the ratchet the floors above imply, stated directly against the same data.
func TestShippedConfigBeatsTheRecording(t *testing.T) {
	entries := loadCorpusFixture(t)
	was := ScoreReplay(recordedConfig, entries)
	now := ScoreReplay(DefaultVerifyConfig, entries)
	t.Logf("recorded: recall %d clean %.4f false %d | shipped: recall %d clean %.4f false %d",
		was.Recall, was.CleanRate(), was.FalseVerifies,
		now.Recall, now.CleanRate(), now.FalseVerifies)
	if now.CleanRate() < was.CleanRate() {
		t.Errorf("clean rate regressed against the recording: %.4f -> %.4f",
			was.CleanRate(), now.CleanRate())
	}
	if now.FalseVerifies > was.FalseVerifies {
		t.Errorf("false verifies grew against the recording: %d -> %d",
			was.FalseVerifies, now.FalseVerifies)
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
