package matcher

// The verifier's thresholds, as data rather than constants.
//
// Every number in here was originally fitted against a benchmark corpus built
// from post-download FILENAMES, which is not what the matcher sees: in
// production it scores Prowlarr release titles at search time. Measured on the
// right corpus (1,570 confirmed search-grabs) the two distributions are not
// close, so each threshold and every "corpus-measured" claim attached to it
// needs re-deriving.
//
// Sweeping them meant a 26-minute bench run against StashDB per arm. Making
// them a struct lets a sweep replay recorded candidates offline instead (see
// replay.go), which turns each arm into microseconds and makes a train/holdout
// split practical rather than aspirational.
//
// Verify keeps its old signature and behaviour by using DefaultVerifyConfig,
// whose values are exactly the constants it used before.
type VerifyConfig struct {
	// RankMinTitleOverlap: for the ranking path, the viewed scene must
	// carry at least this much title overlap, so it is #1 because of the
	// title and not merely the shared performer.
	RankMinTitleOverlap float64
	// TitleMinTokens: the containment path needs a title with at least
	// this many significant tokens, so generic short titles cannot
	// trivially verify everything.
	TitleMinTokens int
	// TitleMinContainment: fraction of the scene title's tokens the
	// release name must contain for the containment path.
	TitleMinContainment float64
	// TitleMinConf: the containment path also requires the viewed scene
	// to already be a real candidate, not a pure title-word coincidence.
	TitleMinConf float64
	// StrongTitleOverlap: a #1 with this much title overlap is the scene
	// even without performer or date signal.
	StrongTitleOverlap float64
	// ShortTitleMaxTokens: at or below this many significant title
	// tokens, Jaccard cannot clear RankMinTitleOverlap even on an exact
	// match, so the ranking path falls back to a confidence check.
	ShortTitleMaxTokens int
	// ShortTitleMinConf: the confidence a short-titled #1 needs to verify
	// through the containment fallback.
	ShortTitleMinConf float64
	// StrongMatchConf: a #1 this confident is the scene even with
	// negligible title overlap.
	//
	// The comment this replaced claimed such confidence "requires
	// performer + date + studio/cast to all agree". It does not: on the
	// search-title corpus, performer (0.40) + studio (0.20) + both tracks
	// + a cast bonus reaches 0.727 with the release date 1,593 days off
	// the scene, and verified the wrong scene. Whether the fix is a higher
	// floor or requiring date corroboration is what the sweep decides.
	StrongMatchConf float64
	// StrongMatchRivalTitleMargin: the strong-match path is blocked when
	// another candidate's title overlap exceeds the viewed scene's by this
	// much, meaning the title IS discriminating between siblings.
	StrongMatchRivalTitleMargin float64
	// DateAnchorMinConf gates the date-anchored path, for the dominant
	// release form that carries no title at all.
	DateAnchorMinConf float64
	// StrongMatchNeedsDate makes the date veto apply to the strong-match
	// path as well as to the title/coincidence paths.
	//
	// Off historically, justified as "corpus-measured: blanket-vetoing it
	// dropped a real 6-performer match with a divergent date and removed
	// zero false verifies". That measurement was taken on the filename
	// corpus and does not carry over; on search titles the strong-match
	// path demonstrably verifies wrong scenes whose dates are years off.
	StrongMatchNeedsDate bool
	// ShortTitleNeedsContainment keeps the short-title fallback requiring
	// the release to actually contain the title's tokens, rather than
	// trusting confidence alone.
	//
	// On by default, which is the shipped behaviour. It exists as a knob so
	// the claim attached to it can be re-measured: the code asserted
	// "corpus-measured: conf-only added false verifies, conf+containment
	// recovers the real short-title scenes with no precision cost", and that
	// measurement was taken on the filename corpus, which is not the input
	// the matcher sees.
	ShortTitleNeedsContainment bool
	// RefuseBehindTheScenes blocks a candidate whose title marks it as a
	// behind-the-scenes cut when the release name carries no such marker.
	//
	// Hand-labelling 40 false verifies found this to be the single largest
	// class, 27% of the sample. A BTS entry shares its main scene's cast,
	// date, studio AND title, differing only by a prefix, so every signal the
	// verifier weighs is identical and no threshold can separate them. The
	// release names one of the two; that is the only discriminating fact
	// available, and it was being ignored.
	//
	// Swept on the search-title corpus: 18 fewer false verifies (98 to 84 on
	// train, 47 to 43 on holdout) for ZERO recall cost on either split. A
	// free improvement, which is why it is on rather than another knob
	// waiting for someone to find it.
	RefuseBehindTheScenes bool
	// TopMargin requires the viewed scene, when verifying through a
	// confidence-driven path, to beat the runner-up candidate by this
	// much. Zero disables the check, which is the historical behaviour.
	//
	// The failure it targets: a performer-only query returns that
	// performer's whole filmography, every entry scoring within a few
	// hundredths of the others (0.727 / 0.714 / 0.672 on one measured
	// case). When the field is that flat, nothing is discriminating and
	// the confident answer is an artefact of the sort order.
	TopMargin float64
}

// DefaultVerifyConfig is the shipped behaviour. Changing a value here changes
// what forage verifies, so a change belongs with the sweep output that
// justifies it.
var DefaultVerifyConfig = VerifyConfig{
	RankMinTitleOverlap:         0.15,
	TitleMinTokens:              4,
	TitleMinContainment:         0.80,
	TitleMinConf:                0.30,
	StrongTitleOverlap:          0.40,
	ShortTitleMaxTokens:         2,
	ShortTitleMinConf:           0.50,
	StrongMatchConf:             0.70,
	StrongMatchRivalTitleMargin: 0.15,
	DateAnchorMinConf:           0.65,
	StrongMatchNeedsDate:        false,
	ShortTitleNeedsContainment:  true,
	RefuseBehindTheScenes:       true,
	TopMargin:                   0,
}
