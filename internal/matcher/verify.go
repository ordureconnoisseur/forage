package matcher

// Verification — deciding whether a specific release IS a specific
// scene — is distinct from ranking (which scene best matches a release).
// The release page and the bench share this one implementation so the
// badge logic is corpus-testable and can't drift between them.

const (
	// verifyRankMinTitleOverlap: for the ranking path, the viewed scene
	// must carry at least this much title overlap (matcher Jaccard) — so
	// it's #1 because of the title, not merely the shared performer. The
	// matcher floors no-overlap at ~0.05; a real partial match is ~0.2+.
	verifyRankMinTitleOverlap = 0.15
	// verifyTitleMinTokens: the containment path needs a title with at
	// least this many significant tokens, so generic short titles ("Home
	// And Horny") — whose words recur across unrelated releases — can't
	// trivially verify everything. Short titles use the ranking path.
	verifyTitleMinTokens = 4
	// verifyTitleMinContainment: fraction of the scene title's tokens the
	// release name must contain for the containment path.
	verifyTitleMinContainment = 0.80
	// verifyTitleMinConf: the containment path also requires the viewed
	// scene to already be a real candidate (performer/date matched, not a
	// pure title-word coincidence — those top out around 0.25).
	verifyTitleMinConf = 0.30
	// verifyStrongTitleOverlap: a #1 with this much title overlap is the
	// scene even without a performer/date signal (the release clearly
	// names it) — recovers correct title-only matches the conf floor
	// would otherwise drop.
	verifyStrongTitleOverlap = 0.40
)

// VerifyResult is the outcome of checking a release against a scene.
type VerifyResult struct {
	Verified   bool
	Confidence float64
}

// Verify decides whether the release (whose matcher candidates are
// `cands`) is the scene identified by sceneID/sceneTitle. Two
// independent signals, either sufficient:
//
//   - ranking: the viewed scene is the matcher's #1 candidate AND that
//     win carries a real title overlap (not just the shared performer).
//   - containment: the release name spells out the viewed scene's
//     (sufficiently distinctive) title AND the scene is already a
//     plausible candidate (performer/date matched).
//
// cands must be Match(releaseName) output; sceneTitle is the viewed
// scene's title (for containment).
func Verify(cands []Candidate, sceneID, sceneTitle, releaseName string) VerifyResult {
	var conf, overlap float64
	found := false
	for i := range cands {
		if cands[i].Scene.ID == sceneID {
			conf = cands[i].Confidence
			overlap = cands[i].TitleOverlap
			found = true
			break
		}
	}

	// Ranking path: viewed scene is the single best, by a real title
	// overlap, AND either a real overall match (conf floor — performer/
	// date matched) or a strong title on its own. The conf floor keeps a
	// title-token coincidence with no performer (conf well under 0.3)
	// from verifying just because it ranked #1 among weak candidates.
	if found && len(cands) > 0 && cands[0].Scene.ID == sceneID &&
		overlap >= verifyRankMinTitleOverlap &&
		(conf >= verifyTitleMinConf || overlap >= verifyStrongTitleOverlap) {
		return VerifyResult{Verified: true, Confidence: conf}
	}

	// Containment path: the release names the scene outright.
	if frac, nTok := TitleContainment(sceneTitle, releaseName); nTok >= verifyTitleMinTokens &&
		frac >= verifyTitleMinContainment && conf >= verifyTitleMinConf {
		if conf < frac {
			conf = frac
		}
		return VerifyResult{Verified: true, Confidence: conf}
	}

	return VerifyResult{Verified: false, Confidence: conf}
}
