package matcher

import "fmt"

// The verifier's reasoning, as data.
//
// Verify answers yes or no, and the release page renders that as a badge. The
// user then has no way to see WHY: there are four acceptance paths and about a
// dozen thresholds behind that one bit, and none of them were visible. The
// bench has had this view for a while (matcher-bench --explain prints the same
// table); this makes the same facts available to the UI.
//
// The explanation is produced by the SAME function that produces the verdict
// (verifyTrace) rather than by a parallel re-implementation, so the two cannot
// disagree — an explanation that drifts from the badge it explains is worse
// than no explanation.

// Gate names. Stable identifiers: the UI keys off them, so renaming one is a
// UI-visible change, not a rename.
const (
	GateRankTitle   = "rank-title"
	GateShortTitle  = "short-title"
	GateStrongMatch = "strong-match"
	GateDateAnchor  = "date-anchor"
	GateContainment = "containment"
)

// VerifyGate is one acceptance path the verifier evaluated. Passed means the
// path's every requirement held; Blockers names the ones that did not, each
// phrased with the measured value AND the threshold it missed, because the
// point is to be readable by someone who has never seen VerifyConfig.
//
// Blockers is only populated on an explained trace. On the verdict-only path
// the gate stops at its first unmet requirement and formats nothing.
type VerifyGate struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Passed   bool     `json:"passed"`
	Blockers []string `json:"blockers,omitempty"`
}

// VerifySignals are the measurements the gates read. Exposed because a number
// often explains a refusal faster than a sentence does: "0.727 against a
// runner-up of 0.714" is the whole story of a flat candidate field.
type VerifySignals struct {
	// Found/Rank describe where the viewed scene landed among the matcher's
	// candidates. Rank is 1-based; 0 means the scene was never retrieved for
	// this release name at all, which is a different failure from losing.
	Found      bool `json:"found"`
	Rank       int  `json:"rank"`
	Candidates int  `json:"candidates"`

	Confidence   float64 `json:"confidence"`
	TitleOverlap float64 `json:"title_overlap"`
	// RunnerUpConf is the best confidence among the OTHER candidates, which is the
	// number the TopMargin guard compares against.
	RunnerUpConf float64 `json:"runner_up_conf"`
	// RivalMaxOverlap is the best cast-stripped title overlap among the other
	// candidates: how much better some sibling's title fits the release.
	RivalMaxOverlap float64 `json:"rival_max_overlap"`
	DateFarOff      bool    `json:"date_far_off"`

	TitleTokens      int     `json:"title_tokens"`
	TitleContainment float64 `json:"title_containment"`
	RivalOwnsTitle   bool    `json:"rival_owns_title"`
	// DateAnchored is only computed when the date-anchor gate is reached (it
	// costs a date scan of the release name), so false can mean "not anchored"
	// or "never asked" on a verdict-only trace. On an explained trace the gate
	// is always evaluated.
	DateAnchored bool `json:"date_anchored"`

	TopSceneID    string  `json:"top_scene_id,omitempty"`
	TopSceneTitle string  `json:"top_scene_title,omitempty"`
	TopConfidence float64 `json:"top_confidence,omitempty"`
}

// VerifyExplanation is the whole trace: the verdict, which path carried it (or
// which veto refused it), and every gate with what stopped it.
type VerifyExplanation struct {
	Verified   bool    `json:"verified"`
	Confidence float64 `json:"confidence"`
	// Path/PathLabel name the gate that accepted. Empty on a refusal.
	Path      string `json:"path,omitempty"`
	PathLabel string `json:"path_label,omitempty"`
	// Veto, when set, refused the release regardless of the gates, so a gate
	// may read as passed while the verdict is no. That combination is exactly
	// what a user needs to see, not a contradiction to hide.
	Veto    string        `json:"veto,omitempty"`
	Gates   []VerifyGate  `json:"gates,omitempty"`
	Signals VerifySignals `json:"signals"`
}

// ExplainVerify is Verify with its reasoning kept. Same verdict, same
// confidence, every gate evaluated instead of stopping at the first answer.
func ExplainVerify(cands []Candidate, sceneID, sceneTitle, releaseName string) VerifyExplanation {
	return ExplainVerifyWith(DefaultVerifyConfig, cands, sceneID, sceneTitle, releaseName)
}

// ExplainVerifyWith is ExplainVerify against an explicit threshold set.
func ExplainVerifyWith(cfg VerifyConfig, cands []Candidate, sceneID, sceneTitle, releaseName string) VerifyExplanation {
	return verifyTrace(cfg, cands, sceneID, sceneTitle, releaseName, true)
}

// GateLabels is every acceptance path's human label, keyed by gate name.
//
// A caller that serialises many traces at once (the release list sends one per
// release, and a search can return a few hundred) can send this table ONCE and
// the names alone per trace. The five labels run to ~300 bytes and are
// identical on every trace, which was measurably the largest avoidable term in
// that payload.
//
// Derived by tracing an empty verification rather than by listing the strings
// again: an explained trace evaluates every gate, so this cannot fall out of
// step with the labels verifyTrace actually emits. Copying them into a second
// table is exactly the drift this file exists to avoid.
func GateLabels() map[string]string {
	ex := ExplainVerify(nil, "", "", "")
	out := make(map[string]string, len(ex.Gates))
	for _, g := range ex.Gates {
		out[g.Name] = g.Label
	}
	return out
}

// verifyCheck is one requirement of one gate: whether it holds, and (when
// explaining) what to tell the user when it does not.
type verifyCheck func() (ok bool, blocker string)

// evalGate runs a gate's requirements in order. Without an explanation it
// stops at the first unmet one: that is the short-circuit the && chains in
// the switch this replaced gave for free, and the reason a threshold sweep
// does not pay for work it will not read.
func evalGate(full bool, name, label string, checks ...verifyCheck) VerifyGate {
	g := VerifyGate{Name: name, Label: label, Passed: true}
	for _, c := range checks {
		ok, blocker := c()
		if ok {
			continue
		}
		g.Passed = false
		if blocker != "" {
			g.Blockers = append(g.Blockers, blocker)
		}
		if !full {
			break
		}
	}
	return g
}

// gateMsg formats blocker text when true and returns "" when false. Verify
// runs per release in interactive searches and over 1,570 corpus entries per
// arm in a threshold sweep; building sentences nobody reads on that path is
// pure waste, and the alternative (a second, explain-only implementation of
// the gates) is the drift this design exists to prevent.
type gateMsg bool

func (m gateMsg) f(format string, a ...any) string {
	if !m {
		return ""
	}
	if len(a) == 0 {
		return format
	}
	return fmt.Sprintf(format, a...)
}

// ratio is the common shape: a measured 0..1 signal that fell short of a
// threshold. Both numbers, because "too low" without the bar is not an
// explanation.
func (m gateMsg) ratio(what string, got, need float64) string {
	return m.f("%s is %.2f, and this path needs %.2f", what, got, need)
}

const farDateBlocker = "the release name carries a date at least two years off this scene's, under every reading of it"

const rivalOwnsTitleBlocker = "the release name spells out a DIFFERENT candidate's title, so it is saying outright which scene it is"

// notTopBlocker distinguishes the two ways a scene fails to be the matcher's
// pick: it lost to something, or it was never retrieved at all. They call for
// different user action (pick the winner vs. search under another name), so
// they must not read the same.
func notTopBlocker(m gateMsg, cands []Candidate, found bool, rank int) string {
	if !m {
		return ""
	}
	if !found {
		if len(cands) == 0 {
			return "the matcher found no scene at all for this release name"
		}
		return m.f("this scene is not among the %d scenes the matcher retrieved for this release name", len(cands))
	}
	top := cands[0]
	return m.f("the matcher ranks this scene #%d of %d, behind %q at %.2f",
		rank, len(cands), top.Scene.Title, top.Confidence)
}

// marginBlocker explains the flat-field refusal: a performer-only query
// returns that performer's whole filmography and every entry scores within a
// few hundredths, so the "winner" is an artefact of the sort order.
func marginBlocker(m gateMsg, cfg VerifyConfig, conf, runnerUp float64) string {
	return m.f("this scene leads the runner-up by only %.3f (%.3f against %.3f), and this path needs %.2f clear",
		conf-runnerUp, conf, runnerUp, cfg.TopMargin)
}

// rivalOverlapBlocker explains the sibling-title refusal: same cast and date
// map to several scenes, and the title is what separates them.
func rivalOverlapBlocker(m gateMsg, rivalOverlap, overlap, margin float64) string {
	return m.f("another candidate's title fits the release name better (%.2f against this scene's %.2f, more than the %.2f this path tolerates), so the title is telling the two apart",
		rivalOverlap, overlap, margin)
}

// weakTitleBlocker covers the rank-title path's last requirement, which is a
// disjunction: enough overall confidence, OR a title strong enough to stand on
// its own. Both halves get named, and the generic-vocabulary case gets its own
// wording because "0.55 overlap is not enough" would otherwise look wrong to
// anyone who knows the 0.40 threshold.
func weakTitleBlocker(m gateMsg, cfg VerifyConfig, conf, overlap float64, sceneTitle, releaseName string) string {
	if !m {
		return ""
	}
	if overlap >= cfg.StrongTitleOverlap && distinctiveTitleHits(sceneTitle, releaseName) == 0 {
		return m.f("the match confidence is %.2f and this path needs %.2f; the title overlap of %.2f would have carried it alone, but every word the release and the title share is generic sex vocabulary, which names no particular scene",
			conf, cfg.TitleMinConf, overlap)
	}
	return m.f("the match confidence is %.2f and this path needs %.2f; the title overlap of %.2f is below the %.2f that would carry the title on its own",
		conf, cfg.TitleMinConf, overlap, cfg.StrongTitleOverlap)
}
