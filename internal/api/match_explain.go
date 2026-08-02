package api

import (
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
// It is built from the candidates verifyReleases ALREADY has in hand — the
// matcher is not re-run to render this. That is why it ships inline with the
// list rather than behind a second endpoint: a per-release "explain" call
// would have to re-query StashDB, which is both slow and free to disagree
// with the badge it is explaining (the scene cache moves under it).

// maxExplainCandidates bounds the candidate table. The matcher returns at most
// ten; five is enough to show a flat field (the 0.727/0.714/0.672 case) and
// keeps the payload proportionate, since this rides along on EVERY release of
// a search that can return a few hundred. The viewed scene is always included
// even when it ranks below the cut — it is the one row the user came for.
const maxExplainCandidates = 5

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
	Overrides  []explainOverride    `json:"overrides,omitempty"`
	Gates      []matcher.VerifyGate `json:"gates,omitempty"`
	Position   explainPosition      `json:"position"`
	Candidates []explainCandidate   `json:"candidates,omitempty"`
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
	Rank         int      `json:"rank"`
	SceneID      string   `json:"scene_id"`
	Title        string   `json:"title"`
	Date         string   `json:"date,omitempty"`
	Studio       string   `json:"studio,omitempty"`
	Cast         []string `json:"cast,omitempty"`
	Confidence   float64  `json:"confidence"`
	TitleOverlap float64  `json:"title_overlap"`
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
	me.Gates = ex.Gates
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
			Confidence:   c.Confidence,
			TitleOverlap: c.TitleOverlap,
			DateFarOff:   c.DateFarOff,
			IsTarget:     c.Scene.ID == sceneID,
		}
		if c.Scene.Studio != nil {
			ec.Studio = c.Scene.Studio.Name
		}
		for _, p := range c.Scene.Performers {
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
