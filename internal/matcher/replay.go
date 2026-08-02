package matcher

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// Replaying recorded matches, so tuning the verifier costs microseconds.
//
// Match() is the expensive half: two StashDB round trips per release, which
// made a full corpus run 26 minutes and put a threshold sweep out of reach.
// Verify() is pure — given the candidates, it touches nothing else — so
// recording each release's candidates once lets every later experiment replay
// them offline.
//
// The dump is also what a CI regression gate needs: it turns "does this change
// hurt accuracy" into a unit test with no network and no API key.

// ReplayPerformer is a candidate scene's cast entry. Both fields matter:
// castStrippedTitleTokens removes them from a rival's title signal.
type ReplayPerformer struct {
	Name string `json:"name"`
	As   string `json:"as,omitempty"`
}

// ReplayCandidate is one candidate, reduced to exactly the fields Verify
// reads. Anything else Match returns (reasons, tracks) is diagnostic and
// deliberately not recorded: it would grow the fixture without changing an
// outcome.
type ReplayCandidate struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Date         string            `json:"date,omitempty"`
	Performers   []ReplayPerformer `json:"performers,omitempty"`
	Confidence   float64           `json:"conf"`
	TitleOverlap float64           `json:"overlap"`
	DateFarOff   bool              `json:"date_far_off,omitempty"`
}

// ReplayEntry is one corpus row: the release the matcher was given, the scene
// it should verify, and what Match returned.
type ReplayEntry struct {
	Release    string            `json:"release"`
	ExpectedID string            `json:"expected_id"`
	Candidates []ReplayCandidate `json:"candidates"`
}

// Candidates rebuilds the matcher-shaped candidate slice. Order is preserved,
// because Verify's isTop reads position 0.
func (e ReplayEntry) Cands() []Candidate {
	out := make([]Candidate, 0, len(e.Candidates))
	for _, c := range e.Candidates {
		sc := stashdb.Scene{ID: c.ID, Title: c.Title, Date: c.Date}
		for _, p := range c.Performers {
			sc.Performers = append(sc.Performers, stashdb.ScenePerformer{Name: p.Name, As: p.As})
		}
		out = append(out, Candidate{
			Scene:        sc,
			Confidence:   c.Confidence,
			TitleOverlap: c.TitleOverlap,
			DateFarOff:   c.DateFarOff,
		})
	}
	return out
}

// expectedTitle returns the recorded title of the expected scene, or "" when
// it was never a candidate. Verify takes the title separately, and passing a
// title for a scene that was not retrieved would let the containment path fire
// on evidence the real caller does not have.
func (e ReplayEntry) expectedTitle() string {
	for _, c := range e.Candidates {
		if c.ID == e.ExpectedID {
			return c.Title
		}
	}
	return ""
}

// RecordCandidates converts live Match output into a replay entry.
func RecordCandidates(release, expectedID string, cands []Candidate) ReplayEntry {
	e := ReplayEntry{Release: release, ExpectedID: expectedID}
	for _, c := range cands {
		rc := ReplayCandidate{
			ID:           c.Scene.ID,
			Title:        c.Scene.Title,
			Date:         c.Scene.Date,
			Confidence:   c.Confidence,
			TitleOverlap: c.TitleOverlap,
			DateFarOff:   c.DateFarOff,
		}
		for _, p := range c.Scene.Performers {
			rc.Performers = append(rc.Performers, ReplayPerformer{Name: p.Name, As: p.As})
		}
		e.Candidates = append(e.Candidates, rc)
	}
	return e
}

// ReplayScore is what a config achieves over a replay set.
//
// Recall and precision move against each other here, so both are always
// reported: a config that verifies nothing has perfect precision, and one that
// verifies everything has perfect recall. Neither is an improvement.
type ReplayScore struct {
	Entries          int
	Recall           int // expected scene verified
	FalseVerifies    int // other candidates that also verified
	EntriesWithFalse int // entries with at least one of those
}

func (s ReplayScore) RecallRate() float64 {
	if s.Entries == 0 {
		return 0
	}
	return float64(s.Recall) / float64(s.Entries)
}

// CleanRate is the fraction of entries that verified the right scene AND
// nothing else. It is the honest single number: an entry where the expected
// scene verifies alongside two wrong ones has not been identified.
func (s ReplayScore) CleanRate() float64 {
	if s.Entries == 0 {
		return 0
	}
	return float64(s.Recall-s.EntriesWithFalse) / float64(s.Entries)
}

// ScoreReplay runs one config over a replay set.
func ScoreReplay(cfg VerifyConfig, entries []ReplayEntry) ReplayScore {
	var s ReplayScore
	s.Entries = len(entries)
	for _, e := range entries {
		cands := e.Cands()
		if VerifyWith(cfg, cands, e.ExpectedID, e.expectedTitle(), e.Release).Verified {
			s.Recall++
		}
		n := 0
		for _, c := range cands {
			if c.Scene.ID == e.ExpectedID {
				continue
			}
			if VerifyWith(cfg, cands, c.Scene.ID, c.Scene.Title, e.Release).Verified {
				n++
			}
		}
		if n > 0 {
			s.FalseVerifies += n
			s.EntriesWithFalse++
		}
	}
	return s
}

// LoadReplay reads a dump written by matcher-bench --dump. A .gz path is
// decompressed, so the committed fixture can be fed straight to verify-sweep
// without an unzip step in between.
func LoadReplay(path string) ([]ReplayEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	var out []ReplayEntry
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveReplay writes a dump, compressing when the path ends in .gz.
//
// The gzip branch exists so refreshing the committed fixture is one command.
// The corpus is ~9 MB of candidate JSON raw and under a megabyte gzipped, and
// the previous refresh recipe (dump plain, gzip by hand, then hand-edit the
// numbers pinned in corpus_gate_test.go) is the kind of ceremony that leaves a
// fixture unrefreshed for a year while the input distribution moves under it.
func SaveReplay(path string, entries []ReplayEntry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	var w io.Writer = f
	var gz *gzip.Writer
	if strings.HasSuffix(path, ".gz") {
		gz = gzip.NewWriter(f)
		w = gz
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(entries); err != nil {
		f.Close()
		return err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// ReplayMeta describes the live Match+Verify run a dump was recorded from.
//
// It exists because the numbers it holds used to be hand-copied constants in
// corpus_gate_test.go. That made refreshing the fixture a two-file edit where
// forgetting the second file leaves the gate asserting a run that no longer
// exists, and it silently discouraged refreshing at all. Recording them beside
// the dump makes a refresh one command and keeps the assertion honest.
//
// Config is the verifier as it stood at record time. Pinning it here rather
// than pointing at DefaultVerifyConfig is deliberate: the faithfulness check
// asks "does replaying reproduce the live run", which is a question about the
// config that ran, not about the config shipping today.
type ReplayMeta struct {
	RecordedAt time.Time `json:"recorded_at"`
	// Commit is the forage build the recording came from, when known. Purely
	// for tracing a surprising number back to a tree.
	Commit string `json:"commit,omitempty"`
	// Corpus names the input set, so a refresh built with different
	// build-corpus flags is visible rather than inferred from the entry count.
	Corpus           string       `json:"corpus,omitempty"`
	Entries          int          `json:"entries"`
	Recall           int          `json:"recall"`
	FalseVerifies    int          `json:"false_verifies"`
	EntriesWithFalse int          `json:"entries_with_false"`
	Config           VerifyConfig `json:"config"`
}

// ReplayMetaPath returns the sidecar path for a dump path: the dump's
// extensions (.json, .json.gz) are replaced with .meta.json, so the pair
// always travels together in one directory.
func ReplayMetaPath(dumpPath string) string {
	base := strings.TrimSuffix(dumpPath, ".gz")
	base = strings.TrimSuffix(base, ".json")
	return base + ".meta.json"
}

// LoadReplayMeta reads a sidecar written by SaveReplayMeta.
func LoadReplayMeta(path string) (ReplayMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ReplayMeta{}, err
	}
	var m ReplayMeta
	// Unknown fields are an error: a sidecar carrying a key this build does
	// not understand is a sidecar written by a different tool version, and
	// quietly ignoring it would let the gate assert against a partially-read
	// recording.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return ReplayMeta{}, err
	}
	return m, nil
}

// SaveReplayMeta writes the sidecar, indented, because it is a committed file
// a human reads in a diff.
func SaveReplayMeta(path string, m ReplayMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
