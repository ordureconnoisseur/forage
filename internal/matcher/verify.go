package matcher

import "strings"

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
	// verifyStrongMatchConf: a #1 candidate this confident is the scene
	// even with negligible title overlap. Title Jaccard collapses when a
	// release pads its name with a long tag list (Brunette, Big Ass, POV,
	// SiteRip, …) or when the scene title carries an episode tag the
	// release omits (e.g. "… For Pussy – S13:E6" vs a release without
	// "S13:E6") — the title is right but drowned. Confidence this high is
	// unreachable by a shared-performer coincidence (performer alone caps
	// at ~0.46); it requires performer + date + studio/cast to all agree,
	// which is identity-level corroboration. Set above that 0.46 ceiling
	// with margin.
	verifyStrongMatchConf = 0.70
	// verifyStrongMatchRivalTitleMargin: the strong-match path is blocked
	// when some OTHER candidate's title overlap exceeds the viewed scene's
	// by this margin. A higher-title-overlap rival means the title IS
	// discriminating between siblings (multi-scene rips where the same
	// cast+date maps to several StashDB scenes — a compilation episode vs
	// its BTS vs a TS-on-TS cut), so we must not let raw confidence
	// override it. When the viewed scene's title is merely drowned by a
	// release's tag-soup, NO rival has meaningfully higher overlap, so the
	// guard doesn't trip and the real match still verifies.
	verifyStrongMatchRivalTitleMargin = 0.15
	// verifyDateAnchorMinConf gates the date-anchored path below. The
	// dominant scene-release form carries NO title at all
	// (site.26.03.20.performer.mp4), so below verifyStrongMatchConf such
	// a release was structurally unverifiable: the overlap and
	// containment paths can never fire on a title that isn't there.
	// performer (0.40) + exact date (0.20) + both-tracks (0.05) lands at
	// ~0.66 even with no studio in the corpus and no cast bonus, so 0.65
	// effectively requires "performer AND exact date agreed" — while the
	// performer-matched-but-date-DISagreeing population sits at ~0.51 and
	// stays correctly out of reach.
	verifyDateAnchorMinConf = 0.65
)

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
	var relTokens map[string]bool // lazily built for rival overlap
	var rivalMaxOverlap float64   // highest CAST-STRIPPED title overlap among OTHER candidates
	for i := range cands {
		if cands[i].Scene.ID == sceneID {
			conf = cands[i].Confidence
			overlap = cands[i].TitleOverlap
			found = true
			continue
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
		if cands[i].Scene.ID == sceneID || cands[i].Confidence < verifyTitleMinConf {
			continue
		}
		rf, rn := TitleContainment(cands[i].Scene.Title, releaseName)
		if rn < verifyTitleMinTokens || rf < verifyTitleMinContainment {
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
		// The strong-title-overlap shortcut verifies on the title ALONE (no
		// performer/date corroboration), so it must only fire when the title
		// names the scene distinctively. A title built entirely from generic
		// sex vocabulary ("Doggy Anal") clears the overlap threshold against
		// any release that lists the same acts but points at no specific
		// scene — require at least one non-generic shared token before
		// trusting the title by itself.
		strongTitle := overlap >= verifyStrongTitleOverlap &&
			distinctiveTitleHits(sceneTitle, releaseName) >= 1
		switch {
		case overlap >= verifyRankMinTitleOverlap &&
			(conf >= verifyTitleMinConf || strongTitle):
			return VerifyResult{Verified: true, Confidence: conf}
		case shortTitle && conf >= verifyShortTitleMinConf && frac >= verifyTitleMinContainment:
			return VerifyResult{Verified: true, Confidence: conf}
		case conf >= verifyStrongMatchConf && !rivalOwnsTitle &&
			rivalMaxOverlap <= overlap+verifyStrongMatchRivalTitleMargin:
			// Title overlap is negligible (tag-soup release name, or an
			// episode tag the release omits) but performer+date+studio/cast
			// corroborate at identity level. Trust the strong overall match —
			// unless a sibling candidate matches the title clearly better,
			// which means the title is discriminating between same-cast
			// scenes and must not be overridden.
			return VerifyResult{Verified: true, Confidence: conf}
		case conf >= verifyDateAnchorMinConf && !rivalOwnsTitle &&
			dateAnchored(cands, sceneID, releaseName) &&
			rivalMaxOverlap <= overlap+verifyStrongMatchRivalTitleMargin:
			// Date-anchored: the release literally states this scene's
			// exact date and NO other candidate shares that date, so the
			// date is doing the discriminating a title would normally do —
			// for releases that carry no title at all. The uniqueness
			// requirement is what keeps same-day multi-site postings (and
			// same-day PMV/compilation siblings) honest: when several
			// candidates share the date it can't separate them, and this
			// path refuses rather than guessing.
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
