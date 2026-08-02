package matcher

import (
	"regexp"
	"strings"
)

// Verification — deciding whether a specific release IS a specific
// scene — is distinct from ranking (which scene best matches a release).
// The release page and the bench share this one implementation so the
// badge logic is corpus-testable and can't drift between them.

// VerifyResult is the outcome of checking a release against a scene.
type VerifyResult struct {
	Verified   bool
	Confidence float64
}

// genericTitleWord lists common sex-act / anatomy / quality vocabulary that
// recurs across unrelated releases. These stay SIGNIFICANT tokens for ranking
// overlap (a scene really can be titled "Doggy Anal", and dropping them from
// titleStopwords would cost recall — see that var's comment); the set is used
// ONLY by the verifier. A title made entirely of these words ("Doggy Anal")
// overlaps any release that names the same acts but identifies no specific
// scene, so the title-ALONE verification path (strong overlap with no
// performer/date corroboration) requires at least one NON-generic shared
// token. Deliberately excludes role/setting words (stepmom, maid, office, …):
// those are frequently the distinctive part of a real title.
var genericTitleWord = map[string]bool{
	"anal": true, "doggy": true, "doggystyle": true, "blowjob": true, "bj": true,
	"handjob": true, "footjob": true, "deepthroat": true, "facial": true,
	"creampie": true, "cumshot": true, "cum": true, "threesome": true,
	"foursome": true, "gangbang": true, "bukkake": true, "blowbang": true,
	"squirt": true, "squirting": true, "fisting": true, "rimming": true,
	"cowgirl": true, "missionary": true, "pov": true, "hardcore": true,
	"sex": true, "fuck": true, "fucking": true, "fucked": true, "fucks": true,
	"pussy": true, "cock": true, "dick": true, "ass": true, "asshole": true,
	"tits": true, "boobs": true, "masturbation": true, "solo": true,
	"lesbian": true, "interracial": true, "bbc": true, "dp": true,
}

// distinctiveTitleHits counts the scene title's significant, NON-generic
// tokens that the release name also contains — the title's discriminating
// signal net of generic sex vocabulary and of release-tag stopwords. One or
// more means the release names the scene by something more specific than the
// acts it depicts, so the title alone can carry a verification.
func distinctiveTitleHits(sceneTitle, releaseName string) int {
	st := filterTitleStopwords(Tokenize(sceneTitle))
	if len(st) == 0 {
		return 0
	}
	rel := tokenSet(filterTitleStopwords(Tokenize(releaseName)))
	seen := map[string]bool{}
	n := 0
	for _, t := range st {
		if genericTitleWord[t] || seen[t] {
			continue
		}
		seen[t] = true
		if rel[t] {
			n++
		}
	}
	return n
}

// dateAnchored reports whether the viewed scene's exact release date is
// (a) the release name's CONVENTIONAL date reading and (b) unique to
// the viewed scene within the candidate set. Both halves matter: (a)
// makes the date an explicit claim by the release rather than a
// coincidence, (b) refuses the case where the date can't discriminate
// (same-performer scenes posted the same day).
//
// "Conventional reading" is deliberate: AllDates emits every plausible
// reading (EU/US swaps, fused member-rip digits) because RANKING treats
// date as a soft, candidate-confirmed signal — but this path makes the
// date decisive, and a speculative reading both inflates confidence
// (bestDateProximity scores that same reading "exact") and satisfies
// the anchor, while the sibling guard can't see a true scene that was
// never retrieved. So the anchor is the dateBetter-preferred reading
// only, and never a fused one (six glued digits are member ids more
// often than dates).
//
// Caveat: the sibling-uniqueness check sees only the candidates Match
// returned (top maxCandidatesReturned); a same-date sibling ranked
// below the cut is invisible. Accepted — dense same-day batches mostly
// fail other requirements too, and the ranking cap keeps Match bounded.
func dateAnchored(cands []Candidate, sceneID, releaseName string) bool {
	var sceneDate string
	for i := range cands {
		if cands[i].Scene.ID == sceneID {
			sceneDate = cands[i].Scene.Date
			break
		}
	}
	if sceneDate == "" {
		return false
	}
	hits := ExtractDates(releaseName)
	if len(hits) == 0 {
		return false
	}
	best := hits[0]
	for _, h := range hits[1:] {
		if dateBetter(h, best) {
			best = h
		}
	}
	if strings.Contains(best.Format, "fused") || best.Date != sceneDate {
		return false
	}
	for i := range cands {
		if cands[i].Scene.ID != sceneID && cands[i].Scene.Date == sceneDate {
			return false
		}
	}
	return true
}

// rivalTitleOverlap measures how well a RIVAL candidate's title matches
// the release, EXCLUDING tokens from the rival's own cast names. The
// rival-title-margin guard exists to detect "the title is discriminating
// between same-cast siblings" — but a rival titled after its performer
// ("Paris Lincoln Solo 4", the solo/intro catalog shape) matches any
// release that names the shared performer, which is cast coincidence,
// not title discrimination. With raw TitleOverlap those rivals
// systematically blocked the strong-match path for title-less releases
// (site.date.performer.mp4 — the dominant scene-release form), where the
// true scene sits at #1 with an exact date and identity-level confidence.
// Same Jaccard shape as titleOverlap, minus the floor (a rival with no
// non-cast signal contributes 0, not minTitleScore).
func rivalTitleOverlap(c Candidate, releaseTokens map[string]bool) float64 {
	if len(releaseTokens) == 0 {
		return 0
	}
	sceneTokens := map[string]bool{}
	for _, t := range castStrippedTitleTokens(c) {
		sceneTokens[t] = true
	}
	if len(sceneTokens) == 0 {
		return 0
	}
	inter := 0
	for t := range sceneTokens {
		if releaseTokens[t] {
			inter++
		}
	}
	union := len(sceneTokens) + len(releaseTokens) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// castStrippedTitleTokens returns a candidate's significant title tokens
// with its OWN cast-name tokens removed — the candidate's title signal
// net of cast coincidence. Shared by the rival overlap and the rival
// containment claim above.
func castStrippedTitleTokens(c Candidate) []string {
	if c.Scene.Title == "" {
		return nil
	}
	cast := map[string]bool{}
	for _, p := range c.Scene.Performers {
		for _, t := range Tokenize(p.Name) {
			cast[t] = true
		}
		if p.As != "" {
			for _, t := range Tokenize(p.As) {
				cast[t] = true
			}
		}
	}
	var out []string
	for _, t := range filterTitleStopwords(Tokenize(c.Scene.Title)) {
		if !cast[t] {
			out = append(out, t)
		}
	}
	return out
}

// btsMarker matches the ways a stash-box titles a behind-the-scenes cut:
// a "BTS" token, or the phrase spelled out. Anchored on token boundaries so
// it cannot fire on a word that merely contains the letters.
var btsMarker = regexp.MustCompile(`(?i)(^|[^a-z])(bts|behind[ ._-]the[ ._-]scenes)([^a-z]|$)`)

// looksBehindTheScenes reports whether a title marks itself as a BTS cut.
func looksBehindTheScenes(title string) bool {
	return btsMarker.MatchString(title)
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
	return VerifyWith(DefaultVerifyConfig, cands, sceneID, sceneTitle, releaseName)
}

// VerifyWith is Verify against an explicit threshold set. Sweeps and the
// replay harness call this; production calls Verify.
func VerifyWith(cfg VerifyConfig, cands []Candidate, sceneID, sceneTitle, releaseName string) VerifyResult {
	ex := verifyTrace(cfg, cands, sceneID, sceneTitle, releaseName, false)
	return VerifyResult{Verified: ex.Verified, Confidence: ex.Confidence}
}

// verifyTrace is the ONE implementation of the verdict. VerifyWith reads the
// summary off it and ExplainVerifyWith returns the whole trace, so the
// explanation the UI shows cannot drift from the badge it explains — the same
// drift risk this file's header calls out between the release page and the
// bench.
//
// full=false reproduces the old short-circuit exactly: gate checks stop at
// their first blocker and gate evaluation stops at the first acceptance, so a
// threshold sweep (1,570 corpus entries per arm) does not start paying for
// dateAnchored and blocker formatting it never displays. full=true evaluates
// everything, which is what makes a refusal legible: the user needs every
// unmet requirement, not just the first one.
func verifyTrace(cfg VerifyConfig, cands []Candidate, sceneID, sceneTitle, releaseName string, full bool) VerifyExplanation {
	var conf, overlap float64
	var targetDateFarOff bool
	found := false
	rank := 0                     // 1-based rank of the viewed scene, 0 = not a candidate
	var relTokens map[string]bool // lazily built for rival overlap
	var rivalMaxOverlap float64   // highest CAST-STRIPPED title overlap among OTHER candidates
	var runnerUpConf float64      // highest CONFIDENCE among OTHER candidates
	for i := range cands {
		if cands[i].Scene.ID == sceneID {
			conf = cands[i].Confidence
			overlap = cands[i].TitleOverlap
			targetDateFarOff = cands[i].DateFarOff
			rank = i + 1
			found = true
			continue
		}
		if cands[i].Confidence > runnerUpConf {
			runnerUpConf = cands[i].Confidence
		}
		if relTokens == nil {
			relTokens = tokenSet(filterTitleStopwords(Tokenize(releaseName)))
		}
		if ov := rivalTitleOverlap(cands[i], relTokens); ov > rivalMaxOverlap {
			rivalMaxOverlap = ov
		}
	}

	frac, nTok := TitleContainment(sceneTitle, releaseName)
	isTop := found && len(cands) > 0 && cands[0].Scene.ID == sceneID

	ex := VerifyExplanation{
		Confidence: conf,
		Signals: VerifySignals{
			Found:            found,
			Rank:             rank,
			Candidates:       len(cands),
			Confidence:       conf,
			TitleOverlap:     overlap,
			RunnerUpConf:     runnerUpConf,
			RivalMaxOverlap:  rivalMaxOverlap,
			DateFarOff:       targetDateFarOff,
			TitleTokens:      nTok,
			TitleContainment: frac,
		},
	}
	if len(cands) > 0 {
		ex.Signals.TopSceneID = cands[0].Scene.ID
		ex.Signals.TopSceneTitle = cands[0].Scene.Title
		ex.Signals.TopConfidence = cands[0].Confidence
	}

	// A BTS cut shares its main scene's cast, date, studio and title, so
	// every weighted signal ties and the ranking is arbitrary. The release
	// naming one of the two is the only fact that separates them.
	if cfg.RefuseBehindTheScenes && looksBehindTheScenes(sceneTitle) &&
		!looksBehindTheScenes(releaseName) {
		ex.Veto = "This scene is a behind-the-scenes cut and the release name does not say BTS. A BTS entry shares its main scene's cast, date, studio and title, so nothing else can separate the two."
		if !full {
			return ex
		}
		// Keep tracing: knowing the release WOULD have verified on cast and
		// date, and was refused only by the BTS rule, is the whole point of
		// showing the user the gates. The veto still decides the verdict.
	}

	// rivalOwnsTitle: some OTHER candidate's distinctive title is spelled
	// out by the release — the release is telling us outright which scene
	// it is, and it isn't the viewed one — so the confidence-driven paths
	// below (strong-match, date-anchored) must refuse. This closes the gap
	// cast-stripping opened: when the true scene's title contains its own
	// cast names ("Afternoon Hookup With Van Wylde"), stripping halves its
	// rival OVERLAP below the margin guard, but its title CLAIM is real.
	//
	// The claim uses the containment path's own conditions on the FULL
	// title, plus one extra requirement: at least two of the rival's
	// NON-cast title tokens must appear in the release. Without that, a
	// rival whose title IS its cast list ("Khloe Kay, Jade Venus & Avery
	// Lust") counts as fully "contained" by any release that names the
	// shared cast — which a six-performer gangbang release does — and
	// that is cast coincidence, not a title claim.
	rivalOwnsTitle := false
	relTokSet := tokenSet(filterTitleStopwords(Tokenize(releaseName)))
	for i := range cands {
		if cands[i].Scene.ID == sceneID || cands[i].Confidence < cfg.TitleMinConf {
			continue
		}
		rf, rn := TitleContainment(cands[i].Scene.Title, releaseName)
		if rn < cfg.TitleMinTokens || rf < cfg.TitleMinContainment {
			continue
		}
		// Count DISTINCT non-cast tokens: castStrippedTitleTokens keeps
		// duplicates, and a single shared word repeated in a rival's title
		// must not satisfy a two-token distinctiveness requirement.
		nonCastHits := map[string]bool{}
		for _, t := range castStrippedTitleTokens(cands[i]) {
			if relTokSet[t] {
				nonCastHits[t] = true
			}
		}
		if len(nonCastHits) >= 2 {
			rivalOwnsTitle = true
			break
		}
	}

	ex.Signals.RivalOwnsTitle = rivalOwnsTitle

	// Ranking path: the viewed scene is the single best pick. Normally we
	// require a real title overlap (so it's #1 for the title, not merely a
	// shared performer), backed by either a real overall match (conf floor)
	// or a strong title on its own. A very short scene title ("Squirt")
	// can't reach the overlap floor even on an exact match — its one or two
	// tokens are a tiny fraction of a release name — so for short titles we
	// fall back to: a strong overall match (high conf = performer AND
	// date/studio agreed) AND the release actually containing the title
	// tokens. Requiring containment (not conf alone) is what keeps a
	// right-performer/wrong-scene release from verifying.
	//
	// Re-measured on the search-title corpus (1,570 entries, the input the
	// matcher actually sees) because the original claim here — "conf-only
	// added false verifies, conf+containment recovers the real short-title
	// scenes with no precision cost" — was taken on a corpus of post-download
	// filenames. What it actually buys: dropping containment costs exactly
	// ONE false verify on the holdout split and zero on train, and changes
	// recall by nothing at all. So the requirement is kept because it is
	// free, not because it is load-bearing, and the fallback is not visibly
	// recovering scenes on this input. Sweep it with
	// ShortTitleNeedsContainment before trusting either reading again.
	//
	// The four ranking paths and the containment path below are the switch
	// cases this used to be, one `gate` call each, in the same order and with
	// each case's conditions in the same order.

	// msg formats blocker text, and only when we are explaining — see the
	// gateMsg comment for why that matters to the sweeps.
	msg := gateMsg(full)
	// gate evaluates one acceptance path and records it. When we are not
	// explaining, evaluation stops at the first path that accepts, exactly as
	// the switch this replaced fell through; the accepted path also sets the
	// verdict, unless a veto already refused.
	accepted := false
	gate := func(name, label string, checks ...verifyCheck) {
		if accepted && !full {
			return
		}
		g := evalGate(full, name, label, checks...)
		ex.Gates = append(ex.Gates, g)
		if !g.Passed || accepted {
			return
		}
		accepted = true
		if ex.Veto != "" {
			return
		}
		ex.Verified = true
		ex.Path = name
		ex.PathLabel = label
		if name == GateContainment && conf < frac {
			// The release literally names the scene, which is stronger
			// evidence than the weighted score it happened to earn.
			ex.Confidence = frac
			ex.Signals.Confidence = frac
		}
	}

	{
		shortTitle := nTok > 0 && nTok <= cfg.ShortTitleMaxTokens
		// The strong-title-overlap shortcut verifies on the title ALONE (no
		// performer/date corroboration), so it must only fire when the title
		// names the scene distinctively. A title built entirely from generic
		// sex vocabulary ("Doggy Anal") clears the overlap threshold against
		// any release that lists the same acts but points at no specific
		// scene — require at least one non-generic shared token before
		// trusting the title by itself.
		strongTitle := func() bool {
			return overlap >= cfg.StrongTitleOverlap &&
				distinctiveTitleHits(sceneTitle, releaseName) >= 1
		}
		// Date veto: a confidently-far release date (>= dateVetoDays off the
		// scene under every plausible reading) vetoes ONLY the two title/
		// coincidence paths below. A same-studio daily episode years off the
		// target clears the title-overlap floor purely because the studio name
		// leaks into title overlap (conf ~0.30) — a far date proves it's a
		// different scene. The strong-match and date-anchored paths are NOT
		// vetoed: strong-match needs conf >= cfg.StrongMatchConf, which
		// tolerates a StashDB-vs-release date discrepancy.
		//
		// The justification here used to read "corpus-measured: blanket-
		// vetoing it dropped a real 6-performer match with a divergent date
		// and removed zero false verifies" — measured on post-download
		// filenames. Re-measured on the 1,570-entry search-title corpus, the
		// veto removes ONE false verify (98 to 97 on train, 47 to 46 on
		// holdout) and costs a little recall on holdout. So "zero" was wrong
		// and the conclusion was right anyway: not worth taking.
		//
		// Note what this does NOT mean. A confident wrong answer here is
		// real: one measured case verified the wrong scene at 0.727 with the
		// release date 1,593 days off. It is rare, not absent. Toggle
		// StrongMatchNeedsDate to re-test.
		dateOK := !targetDateFarOff
		// The confidence-driven paths below answer from corroboration
		// rather than from the release naming the scene, so they are the
		// two that can be fooled by a flat candidate field: a performer-only
		// query returns that performer's whole filmography and every entry
		// scores within a few hundredths. marginOK requires the viewed scene
		// to actually stand out before either path trusts its confidence.
		marginOK := cfg.TopMargin <= 0 || conf >= runnerUpConf+cfg.TopMargin

		// isTopCheck is shared: every ranking path needed `isTop`, which the
		// old code expressed as one enclosing if.
		isTopCheck := func() (bool, string) {
			return isTop, notTopBlocker(msg, cands, found, rank)
		}
		dateCheck := func() (bool, string) { return dateOK, msg.f(farDateBlocker) }

		gate(GateRankTitle, "Ranked first, and the title agrees",
			isTopCheck,
			dateCheck,
			func() (bool, string) {
				return overlap >= cfg.RankMinTitleOverlap,
					msg.ratio("the title overlap with this scene", overlap, cfg.RankMinTitleOverlap)
			},
			func() (bool, string) {
				if conf >= cfg.TitleMinConf || strongTitle() {
					return true, ""
				}
				return false, weakTitleBlocker(msg, cfg, conf, overlap, sceneTitle, releaseName)
			},
		)

		gate(GateShortTitle, "Ranked first, with a title too short to score on overlap",
			isTopCheck,
			dateCheck,
			func() (bool, string) {
				return shortTitle, msg.f(
					"this path only covers titles of %d significant word(s) or fewer, and this one has %d",
					cfg.ShortTitleMaxTokens, nTok)
			},
			func() (bool, string) {
				return conf >= cfg.ShortTitleMinConf,
					msg.ratio("the match confidence", conf, cfg.ShortTitleMinConf)
			},
			func() (bool, string) {
				return !cfg.ShortTitleNeedsContainment || frac >= cfg.TitleMinContainment,
					msg.ratio("the share of the title's words the release name repeats", frac, cfg.TitleMinContainment)
			},
		)

		// Title overlap is negligible (tag-soup release name, or an episode
		// tag the release omits) but performer+date+studio/cast corroborate at
		// identity level. Trust the strong overall match — unless a sibling
		// candidate matches the title clearly better, which means the title is
		// discriminating between same-cast scenes and must not be overridden.
		gate(GateStrongMatch, "Ranked first, and performer/date/studio corroborate on their own",
			isTopCheck,
			func() (bool, string) {
				return conf >= cfg.StrongMatchConf,
					msg.ratio("the match confidence", conf, cfg.StrongMatchConf)
			},
			func() (bool, string) { return !rivalOwnsTitle, msg.f(rivalOwnsTitleBlocker) },
			func() (bool, string) { return marginOK, marginBlocker(msg, cfg, conf, runnerUpConf) },
			func() (bool, string) {
				return dateOK || !cfg.StrongMatchNeedsDate, msg.f(farDateBlocker)
			},
			func() (bool, string) {
				return rivalMaxOverlap <= overlap+cfg.StrongMatchRivalTitleMargin,
					rivalOverlapBlocker(msg, rivalMaxOverlap, overlap, cfg.StrongMatchRivalTitleMargin)
			},
		)

		// Date-anchored: the release literally states this scene's exact date
		// and NO other candidate shares that date, so the date is doing the
		// discriminating a title would normally do — for releases that carry
		// no title at all. The uniqueness requirement is what keeps same-day
		// multi-site postings (and same-day PMV/compilation siblings) honest:
		// when several candidates share the date it can't separate them, and
		// this path refuses rather than guessing.
		gate(GateDateAnchor, "The release states this scene's exact date, and only this scene's",
			isTopCheck,
			func() (bool, string) {
				return conf >= cfg.DateAnchorMinConf,
					msg.ratio("the match confidence", conf, cfg.DateAnchorMinConf)
			},
			func() (bool, string) { return !rivalOwnsTitle, msg.f(rivalOwnsTitleBlocker) },
			func() (bool, string) { return marginOK, marginBlocker(msg, cfg, conf, runnerUpConf) },
			func() (bool, string) {
				anchored := dateAnchored(cands, sceneID, releaseName)
				ex.Signals.DateAnchored = anchored
				return anchored, msg.f("the release name does not read as this scene's exact date, " +
					"or another candidate shares that date and it cannot tell them apart")
			},
			func() (bool, string) {
				return rivalMaxOverlap <= overlap+cfg.StrongMatchRivalTitleMargin,
					rivalOverlapBlocker(msg, rivalMaxOverlap, overlap, cfg.StrongMatchRivalTitleMargin)
			},
		)
	}

	// Containment path: the release names the scene outright. The one path
	// that does NOT require the viewed scene to be ranked first.
	gate(GateContainment, "The release name spells out this scene's title",
		func() (bool, string) {
			return nTok >= cfg.TitleMinTokens, msg.f(
				"this scene's title has %d significant word(s) and this path needs %d, "+
					"so a title this short cannot claim every release it appears in",
				nTok, cfg.TitleMinTokens)
		},
		func() (bool, string) {
			return frac >= cfg.TitleMinContainment,
				msg.ratio("the share of the title's words the release name repeats", frac, cfg.TitleMinContainment)
		},
		func() (bool, string) {
			return conf >= cfg.TitleMinConf,
				msg.ratio("the match confidence", conf, cfg.TitleMinConf)
		},
	)

	return ex
}
