package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/matcher"
)

// The payload budget for the explanation.
//
// This exists because the first version of this feature shipped a size claim
// that was wrong by about 2x. The commit said "3,032 bytes for the worst case
// (all five paths blocked, ten candidates)"; it was measured against a
// hand-written fixture with short cast lists, and "ten candidates" was not even
// a shape the endpoint could emit (maxExplainCandidates is 5). Measured against
// the matcher's own recorded corpus the mean was 2,222 B and the max 5,876 B,
// dominated by an unbounded cast list on one compilation scene with 56
// performers.
//
// That mattered because getSceneReleases attaches an explanation to EVERY
// release and the file's own comment says a search "can return a few hundred".
// So the numbers below are asserted, not written down: a bound nothing checks
// is how this repo got a wrong measurement quoted in a comment for weeks.
//
// Measurement notes, so the next person can judge what these numbers cover:
//   - Input is internal/matcher/testdata/corpus-replay.json.gz, 1,570 recorded
//     releases with their real candidate sets (real titles, dates, cast).
//   - The corpus does NOT record a studio per candidate, so live payloads carry
//     a little more than this measures. The margin between the assertions and
//     the measured values leaves room for it.
//   - The payload is the JSON of ONE sceneRelease.Explain, which is what rides
//     on each release row.

const corpusFixture = "../matcher/testdata/corpus-replay.json.gz"

// loadCorpus reads the matcher's recorded corpus. Absent fixture is a skip, for
// the same reason internal/matcher does it: the fixture holds real scene ids
// from a real library and an owner may keep it out of the tree.
func loadCorpus(t *testing.T) []matcher.ReplayEntry {
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
	var entries []matcher.ReplayEntry
	if err := json.NewDecoder(gz).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

// explainSizes marshals one explanation per corpus entry and returns the sizes,
// sorted. target picks which scene the viewer is looking at.
func explainSizes(t *testing.T, entries []matcher.ReplayEntry, target func(matcher.ReplayEntry, []matcher.Candidate) (string, string)) []int {
	t.Helper()
	var sizes []int
	for _, e := range entries {
		cands := e.Cands()
		if len(cands) == 0 {
			continue
		}
		id, title := target(e, cands)
		me := newMatchExplain(cands, id, title, e.Release, false, nil, "")
		b, err := json.Marshal(me)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(b))
	}
	sort.Ints(sizes)
	return sizes
}

func pct(sizes []int, p float64) int {
	if len(sizes) == 0 {
		return 0
	}
	i := int(float64(len(sizes)-1) * p)
	return sizes[i]
}

func mean(sizes []int) int {
	if len(sizes) == 0 {
		return 0
	}
	sum := 0
	for _, s := range sizes {
		sum += s
	}
	return sum / len(sizes)
}

// TestExplainPayloadStaysWithinBudget measures the real thing against the real
// corpus. The previous claim was measured against a synthetic fixture and was
// wrong by 2x; this is why the numbers are asserted here and not written into a
// comment.
//
// The ceilings sit roughly 15% above the measured values. The slack is for the
// studio names the corpus does not record (~150 B across six rows) plus normal
// wording churn in the blockers. It is small enough that a genuinely unbounded
// field coming back trips it: the pre-fix maxima were 5,876 B and 8,288 B.
func TestExplainPayloadStaysWithinBudget(t *testing.T) {
	entries := loadCorpus(t)

	shapes := []struct {
		name string
		// pick returns the scene the viewer is looking at.
		pick func(matcher.ReplayEntry, []matcher.Candidate) (string, string)
		// Measured 2026-08-02 on the 1,570-entry corpus, after the cast cap,
		// the shared-blocker hoist, the three-decimal rounding, and moving the
		// gate labels to the response.
		maxMean, maxP99, maxAny int
	}{
		{
			// The endpoint's own shape and its common case: the viewer is on
			// the scene the release was recorded as. Measured mean 1,747,
			// p99 2,642, max 3,154 (was mean 2,222 / max 5,876).
			name: "target=expected scene",
			pick: func(e matcher.ReplayEntry, cands []matcher.Candidate) (string, string) {
				for _, c := range cands {
					if c.Scene.ID == e.ExpectedID {
						return c.Scene.ID, c.Scene.Title
					}
				}
				return e.ExpectedID, ""
			},
			maxMean: 2000, maxP99: 3000, maxAny: 3600,
		},
		{
			// The expensive shape, and a real one: the viewed scene ranks
			// last, so every gate fails with its full reasoning and the
			// out-of-cut target adds a sixth candidate row. Measured mean
			// 2,635, p99 3,494, max 3,965 (was max 8,288 per the review).
			name: "target=last candidate",
			pick: func(e matcher.ReplayEntry, cands []matcher.Candidate) (string, string) {
				c := cands[len(cands)-1]
				return c.Scene.ID, c.Scene.Title
			},
			maxMean: 3000, maxP99: 4000, maxAny: 4600,
		},
	}

	for _, s := range shapes {
		sizes := explainSizes(t, entries, s.pick)
		if len(sizes) < 1000 {
			t.Fatalf("%s: only %d payloads measured, the corpus is not being replayed in full", s.name, len(sizes))
		}
		t.Logf("%s: n=%d mean=%d p50=%d p90=%d p99=%d max=%d",
			s.name, len(sizes), mean(sizes), pct(sizes, 0.50),
			pct(sizes, 0.90), pct(sizes, 0.99), sizes[len(sizes)-1])
		if m := mean(sizes); m > s.maxMean {
			t.Errorf("%s: mean payload %d B exceeds the %d B budget; over the %d-release cap that is %d KB",
				s.name, m, s.maxMean, maxExplainedReleases, m*maxExplainedReleases/1024)
		}
		if p := pct(sizes, 0.99); p > s.maxP99 {
			t.Errorf("%s: p99 payload %d B exceeds %d B", s.name, p, s.maxP99)
		}
		if mx := sizes[len(sizes)-1]; mx > s.maxAny {
			t.Errorf("%s: largest payload %d B exceeds the %d B hard ceiling", s.name, mx, s.maxAny)
		}
	}
}

// TestSceneReleasesResponseIsBounded is the finding itself, end to end.
//
// The per-release budget above only matters multiplied by the release count,
// and there was no cap on the release count: the reviewer's arithmetic was
// "~650 KB added to one response, uncompressed". So this builds a 300-release
// response out of real corpus explanations, puts it through the same
// trimExplanations the handler calls and the same gzip level the router now
// installs, and asserts what actually goes over the wire.
//
// What it does NOT cover: the router wiring. That middleware.Compress is
// actually mounted, and applies to this endpoint's content type, is asserted
// separately by TestJSONResponsesAreCompressed.
func TestSceneReleasesResponseIsBounded(t *testing.T) {
	entries := loadCorpus(t)

	var rels []sceneRelease
	for _, e := range entries {
		if len(rels) >= 300 {
			break
		}
		cands := e.Cands()
		if len(cands) == 0 {
			continue
		}
		var title string
		for _, c := range cands {
			if c.Scene.ID == e.ExpectedID {
				title = c.Scene.Title
			}
		}
		rels = append(rels, sceneRelease{
			Title:   e.Release,
			Indexer: "SomeTracker",
			Explain: newMatchExplain(cands, e.ExpectedID, title, e.Release, false, nil, ""),
		})
	}
	if len(rels) < 300 {
		t.Fatalf("only built %d releases", len(rels))
	}

	uncapped := responseBytes(t, rels, len(rels))
	capped := responseBytes(t, rels, maxExplainedReleases)
	t.Logf("300 releases: uncapped raw=%d B gzip=%d B; capped at %d raw=%d B gzip=%d B",
		uncapped.raw, uncapped.gz, maxExplainedReleases, capped.raw, capped.gz)

	// Ceilings measured 2026-08-02: capped raw 290,019 B, gzip 37,366 B.
	const (
		maxRaw  = 340 * 1024
		maxWire = 56 * 1024
	)
	if capped.raw > maxRaw {
		t.Errorf("a 300-release response serialises to %d KB, budget is %d KB", capped.raw/1024, maxRaw/1024)
	}
	if capped.gz > maxWire {
		t.Errorf("a 300-release response is %d KB on the wire after gzip, budget is %d KB", capped.gz/1024, maxWire/1024)
	}
	if capped.raw >= uncapped.raw {
		t.Errorf("the cap did nothing: capped %d B, uncapped %d B", capped.raw, uncapped.raw)
	}
}

type respSize struct{ raw, gz int }

// responseBytes serialises a scene-releases response with the explanations
// trimmed to cap, and reports the size before and after the gzip level the
// router uses.
func responseBytes(t *testing.T, rels []sceneRelease, cap int) respSize {
	t.Helper()
	cp := make([]sceneRelease, len(rels))
	copy(cp, rels)
	trimExplanations(cp, cap)
	resp := sceneReleasesResponse{Releases: cp}
	for i := range cp {
		if cp[i].Explain != nil {
			resp.GateLabels = matcher.GateLabels()
			break
		}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, compressLevel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return respSize{raw: len(b), gz: buf.Len()}
}

// TestTrimExplanationsKeepsTheRankedHead pins the cap's contract: it keeps the
// head of the list, which is only the right thing because the handler ranks
// before calling it. A cap applied to an unranked list would keep whichever
// releases Prowlarr returned first.
func TestTrimExplanationsKeepsTheRankedHead(t *testing.T) {
	rels := make([]sceneRelease, 10)
	for i := range rels {
		rels[i] = sceneRelease{Title: "rel", Explain: &matchExplain{}}
	}
	if got := trimExplanations(rels, 4); got != 6 {
		t.Errorf("dropped %d explanations, expected 6", got)
	}
	for i := range rels {
		if (rels[i].Explain != nil) != (i < 4) {
			t.Errorf("row %d: explain present=%v, expected %v", i, rels[i].Explain != nil, i < 4)
		}
	}
	// Idempotent, and it must not report drops it did not make.
	if got := trimExplanations(rels, 4); got != 0 {
		t.Errorf("second pass reported %d drops", got)
	}
}

// TestExplainCastIsCapped is the specific regression. The 5,876 B worst case
// was one compilation scene whose cast list ran to 56 names, roughly a kilobyte
// inside a single table row. Nothing bounded it, and nothing would have noticed
// if the cap were removed again, so this asserts the cap on the corpus entry
// that actually has the long list rather than on a fixture written to suit it.
func TestExplainCastIsCapped(t *testing.T) {
	entries := loadCorpus(t)

	worstRaw, worstShown := 0, 0
	for _, e := range entries {
		cands := e.Cands()
		for _, c := range cands {
			if n := len(c.Scene.Performers); n > worstRaw {
				worstRaw = n
			}
		}
		for _, ec := range explainCandidates(cands, e.ExpectedID) {
			if n := len(ec.Cast); n > worstShown {
				worstShown = n
			}
			if len(ec.Cast) > maxExplainCast {
				t.Fatalf("candidate %q carries %d cast names, cap is %d", ec.SceneID, len(ec.Cast), maxExplainCast)
			}
		}
	}
	if worstRaw <= maxExplainCast {
		t.Fatalf("the corpus's longest cast list is %d names, at or under the %d cap: "+
			"this test is no longer exercising anything", worstRaw, maxExplainCast)
	}
	t.Logf("longest recorded cast %d names, longest shipped %d", worstRaw, worstShown)
}
