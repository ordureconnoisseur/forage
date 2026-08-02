package matcher

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
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

// corpusFixtureMeta is the sidecar recording of the live run the fixture came
// from: entry counts, the live Match+Verify result, and the verifier config
// that produced it. Written by `matcher-bench --verify --dump`, so refreshing
// the fixture refreshes the assertions below in the same command.
var corpusFixtureMeta = ReplayMetaPath(corpusFixture)

// The fixture is 1,570 entries of roughly ten candidates each, so one
// ScoreReplay is ~16,000 Verify calls. CI runs `go test -race` and nothing
// else, so these tests cannot opt out of the race detector — which makes them
// 10-50x slower and blew the package's 10-minute timeout when four tests each
// re-did the same work. Load once, score each distinct config once.
var (
	fixtureOnce    sync.Once
	fixtureEntries []ReplayEntry
	fixtureErr     error

	scoreMu    sync.Mutex
	scoreCache = map[string]ReplayScore{}
)

// scoreOnce memoises ScoreReplay per named config. The name is the key
// because VerifyConfig is comparable but a map keyed on it would silently
// recompute whenever a field is added.
func scoreOnce(name string, cfg VerifyConfig, entries []ReplayEntry) ReplayScore {
	scoreMu.Lock()
	defer scoreMu.Unlock()
	if s, ok := scoreCache[name]; ok {
		return s
	}
	s := ScoreReplay(cfg, entries)
	scoreCache[name] = s
	return s
}

// loadCorpusFixture returns the recorded corpus, or nil when the fixture is
// absent. Absence is a skip rather than a failure: the fixture holds real
// scene ids from a real library, so it is the kind of thing an owner may want
// out of the tree, and that choice must not break the build.
func loadCorpusFixture(t *testing.T) []ReplayEntry {
	t.Helper()
	fixtureOnce.Do(func() {
		f, err := os.Open(corpusFixture)
		if err != nil {
			fixtureErr = err
			return
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			fixtureErr = err
			return
		}
		defer gz.Close()
		fixtureErr = json.NewDecoder(gz).Decode(&fixtureEntries)
	})
	if errors.Is(fixtureErr, fs.ErrNotExist) {
		t.Skip("no corpus fixture; regenerate with matcher-bench --verify --dump")
		return nil
	}
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return fixtureEntries
}

func TestCorpusAccuracyDoesNotRegress(t *testing.T) {
	entries := loadCorpusFixture(t)
	if len(entries) < 1000 {
		t.Fatalf("fixture holds %d entries, expected the full corpus", len(entries))
	}
	s := scoreOnce("default", DefaultVerifyConfig, entries)
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

// loadCorpusMeta returns the fixture's sidecar recording: the live run's
// numbers and the verifier config that produced them.
//
// Those values used to be constants in this file, hand-copied from a bench
// run's stdout. That made a fixture refresh a two-file edit, and the half
// nobody remembers is this one: a refreshed fixture with stale constants fails
// loudly, which teaches the next person not to refresh. Reading the recording
// that the dump wrote alongside itself removes the choice.
func loadCorpusMeta(t *testing.T) ReplayMeta {
	t.Helper()
	m, err := LoadReplayMeta(corpusFixtureMeta)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("no fixture sidecar at %s; regenerate with matcher-bench --verify --dump", corpusFixtureMeta)
	}
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The fixture must reproduce the live bench, run under the config that
// produced it. If replaying diverges, every offline experiment is measuring a
// different verifier from the one that ran, and this whole apparatus is
// decorative.
//
// The comparison uses the sidecar's recorded config, not DefaultVerifyConfig,
// which was the first version's mistake: it made this check fail whenever the
// verifier legitimately improved, so it was really asserting "nobody has
// changed anything" while claiming to assert "the replay is faithful". Those
// are different questions and only the second is useful.
func TestCorpusFixtureMatchesTheLiveRun(t *testing.T) {
	entries := loadCorpusFixture(t)
	meta := loadCorpusMeta(t)
	s := scoreOnce("recorded", meta.Config, entries)
	if s.Entries != meta.Entries || s.Recall != meta.Recall ||
		s.FalseVerifies != meta.FalseVerifies || s.EntriesWithFalse != meta.EntriesWithFalse {
		t.Errorf("replay diverged from the live run recorded %s:\n  got  entries=%d recall=%d false=%d entriesWithFalse=%d\n  want entries=%d recall=%d false=%d entriesWithFalse=%d",
			meta.RecordedAt.Format("2006-01-02"),
			s.Entries, s.Recall, s.FalseVerifies, s.EntriesWithFalse,
			meta.Entries, meta.Recall, meta.FalseVerifies, meta.EntriesWithFalse)
	}
}

// The sidecar must describe every knob the verifier has.
//
// Adding a VerifyConfig field and leaving the sidecar alone gives the recorded
// config that field's zero value, so the faithfulness check above would replay
// a verifier nobody ever ran: it would either fail for a reason its message
// cannot explain, or pass by luck when the zero value happens to be the
// shipped one. Comparing key sets says the real thing instead: the recording
// predates the knob, re-record it.
func TestFixtureSidecarDescribesEveryVerifierKnob(t *testing.T) {
	raw, err := os.ReadFile(corpusFixtureMeta)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("no fixture sidecar at %s", corpusFixtureMeta)
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	shipped, err := json.Marshal(DefaultVerifyConfig)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(shipped, &want); err != nil {
		t.Fatal(err)
	}
	for k := range want {
		if _, ok := doc.Config[k]; !ok {
			t.Errorf("sidecar config is missing %q: the verifier grew a knob since the fixture was recorded, so the recording no longer describes a verifier that ran. Re-record with `make bench-refresh`.", k)
		}
	}
	for k := range doc.Config {
		if _, ok := want[k]; !ok {
			t.Errorf("sidecar config carries %q, which VerifyConfig no longer has: re-record the fixture", k)
		}
	}
}

// The fixture is a snapshot of a moving distribution: StashDB gains and edits
// scenes, and the corpus is rebuilt from the user's confirmed grabs, whose mix
// of indexers and release groups shifts. A gate replaying a two-year-old
// recording still passes, and still says nothing about today's input.
//
// Opt-in via env rather than always-on, because a test that turns red on a
// calendar date breaks the build for someone who changed nothing, and a build
// that goes red for no reason is a build people learn to force through. The
// weekly scheduled workflow sets the variable; push CI does not.
func TestReplayFixtureIsNotStale(t *testing.T) {
	v := os.Getenv("FORAGE_FIXTURE_MAX_AGE_DAYS")
	if v == "" {
		t.Skip("FORAGE_FIXTURE_MAX_AGE_DAYS unset; freshness is checked by the scheduled bench workflow")
	}
	maxDays, err := strconv.Atoi(v)
	if err != nil || maxDays <= 0 {
		t.Fatalf("FORAGE_FIXTURE_MAX_AGE_DAYS=%q is not a positive integer", v)
	}
	meta := loadCorpusMeta(t)
	ageDays := time.Since(meta.RecordedAt).Hours() / 24
	t.Logf("fixture recorded %s, %.0f days ago", meta.RecordedAt.Format("2006-01-02"), ageDays)
	if ageDays > float64(maxDays) {
		t.Errorf("corpus fixture is %.0f days old, budget is %d: the frozen gate no longer resembles current matcher input. Re-record with `make bench-refresh`.",
			ageDays, maxDays)
	}
}

// And the shipped verifier must be at least as good as the recording. This is
// the ratchet the floors above imply, stated directly against the same data.
func TestShippedConfigBeatsTheRecording(t *testing.T) {
	entries := loadCorpusFixture(t)
	was := scoreOnce("recorded", loadCorpusMeta(t).Config, entries)
	now := scoreOnce("default", DefaultVerifyConfig, entries)
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
	// A slice, not the whole corpus. This asserts the gate is capable of
	// failing in each direction, which a few hundred entries demonstrates as
	// well as 1,570 — and the accept-everything arm verifies every candidate
	// of every entry, which is the single most expensive thing in the
	// package. Measured at 2m20s under -race on the full set.
	if len(entries) > 300 {
		entries = entries[:300]
	}

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
