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
	// verifyShortTitleMaxTokens: at or below this many significant title
	// tokens, Jaccard overlap can't clear verifyRankMinTitleOverlap even on
	// an exact match — one or two tokens are a tiny fraction of a full
	// release name — so the ranking path falls back to a confidence check
	// instead of the overlap floor for such titles.
	verifyShortTitleMaxTokens = 2
	// verifyShortTitleMinConf: the confidence a short-titled #1 needs to
	// verify via the containment fallback (below). Set at the performer+
	// date / performer+studio level so a bare shared-performer coincidence
	// (conf ~0.4) doesn't qualify.
	verifyShortTitleMinConf = 0.50
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

	frac, nTok := TitleContainment(sceneTitle, releaseName)
	isTop := found && len(cands) > 0 && cands[0].Scene.ID == sceneID

	// Ranking path: the viewed scene is the single best pick. Normally we
	// require a real title overlap (so it's #1 for the title, not merely a
	// shared performer), backed by either a real overall match (conf floor)
	// or a strong title on its own. A very short scene title ("Squirt")
	// can't reach the overlap floor even on an exact match — its one or two
	// tokens are a tiny fraction of a release name — so for short titles we
	// fall back to: a strong overall match (high conf = performer AND
	// date/studio agreed) AND the release actually containing the title
	// tokens. Requiring containment (not conf alone) is what keeps a
	// right-performer/wrong-scene release from verifying — corpus-measured:
	// conf-only added false verifies, conf+containment recovers the real
	// short-title scenes with no precision cost.
	if isTop {
		shortTitle := nTok > 0 && nTok <= verifyShortTitleMaxTokens
		switch {
		case overlap >= verifyRankMinTitleOverlap &&
			(conf >= verifyTitleMinConf || overlap >= verifyStrongTitleOverlap):
			return VerifyResult{Verified: true, Confidence: conf}
		case shortTitle && conf >= verifyShortTitleMinConf && frac >= verifyTitleMinContainment:
			return VerifyResult{Verified: true, Confidence: conf}
		}
	}

	// Containment path: the release names the scene outright.
	if nTok >= verifyTitleMinTokens && frac >= verifyTitleMinContainment && conf >= verifyTitleMinConf {
		if conf < frac {
			conf = frac
		}
		return VerifyResult{Verified: true, Confidence: conf}
	}

	return VerifyResult{Verified: false, Confidence: conf}
}
