package api

import (
	"math"

	"github.com/ordureconnoisseur/forager/internal/matcher"
)

// The "why this verdict" payload for one release.
//
// The release list shows a verdict badge and, until now, nothing else: the
// four acceptance paths and dozen thresholds behind that badge were invisible,
// so a wrong answer was indistinguishable from a right one. The failure that
// motivated this: a release verified the WRONG scene at 0.727 confidence with
// its date 1,593 days off, and the top three candidates were separated by
// 0.013. Every fact needed to spot that was already computed and thrown away.
//
// It is built from the candidates verifyReleases ALREADY has in hand, and the
// matcher is not re-run to render this. That is why it ships inline with the
// list rather than behind a second endpoint: a per-release "explain" call
// would have to re-query StashDB, which is both slow and free to disagree
// with the badge it is explaining (the scene cache moves under it).

// maxExplainCandidates bounds the candidate table. The matcher returns at most
// ten; five is enough to show a flat field (the 0.727/0.714/0.672 case) and
// keeps the payload proportionate, since this rides along on EVERY release of
// a search that can return a few hundred. The viewed scene is always included
// even when it ranks below the cut: it is the one row the user came for.
const maxExplainCandidates = 5

// maxExplainCast bounds the names listed per candidate row.
//
// This is the single biggest term in the payload and it used to be unbounded.
// Measured over the matcher's recorded corpus (see match_explain_size_test.go):
// one compilation scene carries 56 performers, roughly a kilobyte inside one
// table row, and that row is what made the worst case 5,876 B rather than the
// 3,032 B the first version of this commit claimed. Four names plus a count is
// enough to tell two same-cast siblings apart, which is the only thing the cast
// column is for.
const maxExplainCast = 4

// minSharedBlockerGates is how many failing paths must name the same blocker
// before it is hoisted out of them and stated once.
//
// The case this exists for is "not the top candidate", which blocks four of the
// five paths (containment is the one that does not need the top spot), so the
// same sentence repeated four times buried each path's own reason. Three, not
// two: with three failing gates a two-gate threshold would hoist a blocker that
// is not actually shared by most of them, and a hoisted line reads as a general
// fact about the release.
const minSharedBlockerGates = 3

// matchExplain is the whole decision for one release: the matcher's ranked
// candidates, the gate trace behind the verdict, and any override forager
// applied on top of the matcher's answer.
type matchExplain struct {
	// Verified is the FINAL verdict, after the overrides below. It can differ
	// from the matcher gate trace's own answer, which is the point of showing
	// both.
	Verified  bool   `json:"verified"`
	Path      string `json:"path,omitempty"`
	PathLabel string `json:"path_label,omitempty"`
	Veto      string `json:"veto,omitempty"`
	// Note carries the cases with no gate trace at all: the matcher errored,
	// or it retrieved no candidate scenes for this release name.
	Note string `json:"note,omitempty"`
	// Overrides are forage's own rules layered on the matcher's verdict (pack,
	// link spam, image set, JAV code). Without them the UI would report
	// "verified via strong match" next to an unverified badge.
	Overrides []explainOverride `json:"overrides,omitempty"`
	// SharedBlockers are the reasons at least minSharedBlockerGates of the
	// failing paths gave, lifted out of them and stated once. Hoisted HERE
	// rather than in the UI so the rule is testable and so the same sentence
	// is not serialised four times on every release of every search.
	SharedBlockers []string           `json:"shared_blockers,omitempty"`
	Gates          []explainGate      `json:"gates,omitempty"`
	Position       explainPosition    `json:"position"`
	Candidates     []explainCandidate `json:"candidates,omitempty"`
}

// explainGate is one acceptance path as the UI needs it. Deliberately NOT
// matcher.VerifyGate: that carries the human label, which is the same ~60-byte
// string on every release of the response. The labels ship once per response as
// sceneReleasesResponse.GateLabels, from matcher.GateLabels(), and the name is
// the key into it. Gate names are already documented as the UI's stable
// identifiers, so this adds no new contract.
type explainGate struct {
	Name     string   `json:"name"`
	Passed   bool     `json:"passed"`
	Blockers []string `json:"blockers,omitempty"`
}

// explainPosition is where the viewed scene landed in the field. Only these
// three of the verifier's signals are projected: every other measurement it
// takes is already quoted inside the gate blockers that turned on it, and this
// payload rides along on every release of a search that can return hundreds.
type explainPosition struct {
	// Found distinguishes "lost to something" from "never retrieved", which
	// call for different user action (pick the winner vs. search under another
	// name).
	Found      bool `json:"found"`
	Rank       int  `json:"rank"`
	Candidates int  `json:"candidates"`
}

// explainOverride is one forage-level rule that changed the verdict after the
// matcher had spoken. Verdict is "verified" or "refused" so the UI can colour
// it without parsing the sentence.
type explainOverride struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// explainCandidate is one scene the matcher considered, with what it scored.
// Cast and date are here because they are what a person actually compares when
// deciding whether the matcher picked the right scene.
type explainCandidate struct {
	Rank    int    `json:"rank"`
	SceneID string `json:"scene_id"`
	Title   string `json:"title"`
	Date    string `json:"date,omitempty"`
	Studio  string `json:"studio,omitempty"`
	// Cast is capped at maxExplainCast; CastMore is how many names were left
	// out, so the UI can say "+52 more" rather than silently shortening a
	// cast list the user is comparing against.
	Cast     []string `json:"cast,omitempty"`
	CastMore int      `json:"cast_more,omitempty"`
	// Confidence/TitleOverlap are rounded to three decimals. The UI renders
	// them as a whole percentage and as 2dp, so full float64 precision put
	// about 170 bytes of trailing digits on every release row and showed
	// nobody anything.
	Confidence   float64 `json:"confidence"`
	TitleOverlap float64 `json:"title_overlap"`
	// DateFarOff: this candidate's date is at least two years off the release
	// name's, under every reading of it.
	DateFarOff bool `json:"date_far_off,omitempty"`
	// IsTarget marks the scene the user is looking at.
	IsTarget bool `json:"is_target,omitempty"`
}

// newMatchExplain builds the payload from candidates already in hand.
// verified/overrides are the FINAL verdict after forage's own rules, which the
// matcher knows nothing about.
func newMatchExplain(cands []matcher.Candidate, sceneID, sceneTitle, releaseName string, verified bool, overrides []explainOverride, note string) *matchExplain {
	me := &matchExplain{Verified: verified, Overrides: overrides, Note: note}
	if len(cands) == 0 {
		return me
	}
	ex := matcher.ExplainVerifyWith(matcher.DefaultVerifyConfig, cands, sceneID, sceneTitle, releaseName)
	me.Path = ex.Path
	me.PathLabel = ex.PathLabel
	me.Veto = ex.Veto
	me.SharedBlockers, me.Gates = hoistSharedBlockers(ex.Gates)
	me.Position = explainPosition{
		Found:      ex.Signals.Found,
		Rank:       ex.Signals.Rank,
		Candidates: ex.Signals.Candidates,
	}
	me.Candidates = explainCandidates(cands, sceneID)
	return me
}

// explainCandidates shapes the ranked candidate list for the UI, capped, with
// the viewed scene always present so "why not the one I expected" is always
// answerable.
func explainCandidates(cands []matcher.Candidate, sceneID string) []explainCandidate {
	shape := func(i int) explainCandidate {
		c := cands[i]
		ec := explainCandidate{
			Rank:         i + 1,
			SceneID:      c.Scene.ID,
			Title:        c.Scene.Title,
			Date:         c.Scene.Date,
			Confidence:   round3(c.Confidence),
			TitleOverlap: round3(c.TitleOverlap),
			DateFarOff:   c.DateFarOff,
			IsTarget:     c.Scene.ID == sceneID,
		}
		if c.Scene.Studio != nil {
			ec.Studio = c.Scene.Studio.Name
		}
		for _, p := range c.Scene.Performers {
			if len(ec.Cast) >= maxExplainCast {
				ec.CastMore++
				continue
			}
			ec.Cast = append(ec.Cast, p.Name)
		}
		return ec
	}

	n := len(cands)
	if n > maxExplainCandidates {
		n = maxExplainCandidates
	}
	out := make([]explainCandidate, 0, n+1)
	for i := 0; i < n; i++ {
		out = append(out, shape(i))
	}
	for i := n; i < len(cands); i++ {
		if cands[i].Scene.ID == sceneID {
			out = append(out, shape(i))
			break
		}
	}
	return out
}

// round3 trims a 0..1 signal to three decimals. See explainCandidate.
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// hoistSharedBlockers lifts the reasons most failing paths agree on out of
// those paths and returns them separately, alongside the gates with those
// reasons removed.
//
// Two things are going on. Legibility: "the matcher ranks this scene #2 of 7,
// behind X at 0.81" repeated down four paths buries what is specific to each
// one. And size: that sentence is ~90 bytes, sent four times, on every release
// of a search that can return a few hundred.
//
// A gate can end up with every blocker hoisted. That is left as-is on purpose
// rather than held back one: the gate still renders, and the UI says the shared
// reason above is the only thing that stopped it. Suppressing the hoist to
// avoid an empty list would put the sentence back four times.
func hoistSharedBlockers(gates []matcher.VerifyGate) ([]string, []explainGate) {
	var failing int
	counts := map[string]int{}
	var order []string
	for _, g := range gates {
		if g.Passed {
			continue
		}
		failing++
		seen := map[string]bool{}
		for _, b := range g.Blockers {
			if seen[b] {
				continue
			}
			seen[b] = true
			if counts[b] == 0 {
				order = append(order, b)
			}
			counts[b]++
		}
	}
	shared := map[string]bool{}
	var out []string
	if failing >= minSharedBlockerGates {
		for _, b := range order {
			if counts[b] >= minSharedBlockerGates {
				shared[b] = true
				out = append(out, b)
			}
		}
	}
	trimmed := make([]explainGate, 0, len(gates))
	for _, g := range gates {
		eg := explainGate{Name: g.Name, Passed: g.Passed, Blockers: g.Blockers}
		if len(shared) > 0 && !g.Passed && len(g.Blockers) > 0 {
			kept := make([]string, 0, len(g.Blockers))
			for _, b := range g.Blockers {
				if !shared[b] {
					kept = append(kept, b)
				}
			}
			if len(kept) == 0 {
				kept = nil
			}
			eg.Blockers = kept
		}
		trimmed = append(trimmed, eg)
	}
	return out, trimmed
}
