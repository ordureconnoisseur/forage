package matcher

import (
	"math/rand"
	"testing"
)

// The faithfulness oracle for the switch-to-gates refactor.
//
// This exists because the branch's first attempt at proving the refactor
// faithful was circular. TestExplainAgreesWithVerifyOnCorpus compares
// VerifyWith against ExplainVerifyWith, and since the refactor those are
// verifyTrace(full=false) and verifyTrace(full=true): ONE implementation in two
// modes. A condition transcribed wrong out of the old switch is present
// identically in both arms, so that test stays green while the verdict the
// whole library depends on has changed. It catches short-circuit and gate-order
// divergence, which is worth having, and nothing else.
//
// So the old code is transcribed back in, verbatim, and used as the reference.
// verifyWithAsOfMain below is a copy of VerifyWith as it stood at
// 5834421 (origin/main, "matcher: re-measure the claims..."), the commit this
// branch was cut from, taken with `git show origin/main:internal/matcher/verify.go`
// and reduced only by deleting comments. Its helper calls (dateAnchored,
// rivalTitleOverlap, castStrippedTitleTokens, distinctiveTitleHits,
// TitleContainment, looksBehindTheScenes) resolve to the SAME functions the new
// code uses, and the diff against main confirms this branch did not touch any of
// them: the only removed lines in verify.go are the switch and the two return
// statements around it. So this compares decision logic to decision logic,
// which is exactly the claim under test, and nothing more.
//
// WHAT THIS DOES NOT PROVE: that the old code was right. It pins the refactor
// to the behaviour of the commit this branch forked from. If main's verifier
// changes, this copy must be re-taken from main or the test becomes a pin on a
// stale verifier that quietly blocks a legitimate improvement. That is the
// mistake recordedConfig's comment in corpus_gate_test.go describes, and this
// file is the same shape of thing, so it carries the same hazard.
func verifyWithAsOfMain(cfg VerifyConfig, cands []Candidate, sceneID, sceneTitle, releaseName string) VerifyResult {
	var conf, overlap float64
	var targetDateFarOff bool
	found := false
	var relTokens map[string]bool
	var rivalMaxOverlap float64
	var runnerUpConf float64
	for i := range cands {
		if cands[i].Scene.ID == sceneID {
			conf = cands[i].Confidence
			overlap = cands[i].TitleOverlap
			targetDateFarOff = cands[i].DateFarOff
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

	if cfg.RefuseBehindTheScenes && looksBehindTheScenes(sceneTitle) &&
		!looksBehindTheScenes(releaseName) {
		return VerifyResult{Verified: false, Confidence: conf}
	}

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

	if isTop {
		shortTitle := nTok > 0 && nTok <= cfg.ShortTitleMaxTokens
		strongTitle := overlap >= cfg.StrongTitleOverlap &&
			distinctiveTitleHits(sceneTitle, releaseName) >= 1
		dateOK := !targetDateFarOff
		marginOK := cfg.TopMargin <= 0 || conf >= runnerUpConf+cfg.TopMargin
		switch {
		case dateOK && overlap >= cfg.RankMinTitleOverlap &&
			(conf >= cfg.TitleMinConf || strongTitle):
			return VerifyResult{Verified: true, Confidence: conf}
		case dateOK && shortTitle && conf >= cfg.ShortTitleMinConf &&
			(!cfg.ShortTitleNeedsContainment || frac >= cfg.TitleMinContainment):
			return VerifyResult{Verified: true, Confidence: conf}
		case conf >= cfg.StrongMatchConf && !rivalOwnsTitle && marginOK &&
			(dateOK || !cfg.StrongMatchNeedsDate) &&
			rivalMaxOverlap <= overlap+cfg.StrongMatchRivalTitleMargin:
			return VerifyResult{Verified: true, Confidence: conf}
		case conf >= cfg.DateAnchorMinConf && !rivalOwnsTitle && marginOK &&
			dateAnchored(cands, sceneID, releaseName) &&
			rivalMaxOverlap <= overlap+cfg.StrongMatchRivalTitleMargin:
			return VerifyResult{Verified: true, Confidence: conf}
		}
	}

	if nTok >= cfg.TitleMinTokens && frac >= cfg.TitleMinContainment && conf >= cfg.TitleMinConf {
		if conf < frac {
			conf = frac
		}
		return VerifyResult{Verified: true, Confidence: conf}
	}

	return VerifyResult{Verified: false, Confidence: conf}
}

// oracleConfig is one threshold set to compare under. explain says whether the
// explained arm is compared too: that arm costs about half again as much (it
// evaluates every gate and formats every blocker), and the property it adds
// over the verdict-only arm is mode agreement, which
// TestExplainAgreesWithVerifyOnCorpus already sweeps. It is kept on the two
// configs where the veto and the margin guard interact with the trace, and
// dropped elsewhere to keep this test inside CI's budget (see the runtime note
// on TestVerifyMatchesPreRefactorOracle).
type oracleConfig struct {
	name    string
	cfg     VerifyConfig
	explain bool
}

// oracleConfigs are the threshold sets the comparison runs under.
//
// DefaultVerifyConfig alone is not enough, and that gap is what let the first
// version of this work ship unproven. The one exact-count assertion in the tree
// (TestCorpusFixtureMatchesTheLiveRun) runs under recordedConfig, which sets
// RefuseBehindTheScenes:false and TopMargin:0, so the BTS veto and the margin
// guard, the two things the refactor moved MOST (the veto now keeps tracing
// instead of returning, and marginOK became a check inside two gates), were
// covered by nothing. Each config below turns on something the others leave
// off. recordedConfig is deliberately absent: field for field it is
// DefaultVerifyConfig with RefuseBehindTheScenes off, which is the "no-bts"
// entry, and running it twice buys nothing but wall clock.
var oracleConfigs = func() []oracleConfig {
	// TopMargin is 0 in both DefaultVerifyConfig and recordedConfig, so
	// marginOK is unconditionally true everywhere else in the tree and the
	// margin check inside strong-match and date-anchor is never exercised.
	margin := DefaultVerifyConfig
	margin.TopMargin = 0.05

	needsDate := DefaultVerifyConfig
	needsDate.StrongMatchNeedsDate = true

	noBTS := DefaultVerifyConfig
	noBTS.RefuseBehindTheScenes = false

	// Loose thresholds: several gates accept that would not under the
	// default, which exercises the "first gate to accept wins" ordering
	// rather than only the refusal paths.
	loose := DefaultVerifyConfig
	loose.RankMinTitleOverlap = 0.05
	loose.TitleMinConf = 0.10
	loose.StrongMatchConf = 0.40
	loose.DateAnchorMinConf = 0.30
	loose.TitleMinContainment = 0.50
	loose.TitleMinTokens = 2
	loose.ShortTitleMaxTokens = 4
	loose.ShortTitleMinConf = 0.20

	return []oracleConfig{
		{"default", DefaultVerifyConfig, true},
		{"bts+margin", margin, true},
		{"strong-match-needs-date", needsDate, false},
		{"no-bts", noBTS, false},
		{"loose", loose, false},
	}
}()

// TestVerifyMatchesPreRefactorOracle is the actual faithfulness proof.
//
// Every candidate of every recorded corpus entry is verified twice: once by the
// shipped verifier, once by the pre-refactor copy above. Both the verdict and
// the confidence must agree, under every config in oracleConfigs. The explained
// path is compared too, so the panel and the badge and the old code all have to
// land in the same place.
//
// A gate whose conditions were transcribed in the wrong ORDER usually still
// agrees (the conditions are pure predicates on precomputed values); what this
// catches is a condition transcribed WRONG, dropped, inverted, or attached to
// the wrong gate. That is the class TestExplainAgreesWithVerifyOnCorpus is
// structurally unable to see.
//
// Runtime: 15,700 verdicts per pass, ~2.7s a pass locally, 12 passes across the
// configs below. The subtests run in parallel because CI runs the whole tree
// under -race with a 25-minute budget and this package's own comment records
// that it has blown a timeout before. Measured: the package goes from 12.8s to
// 30.6s without -race with this file and its two siblings present.
func TestVerifyMatchesPreRefactorOracle(t *testing.T) {
	entries := loadCorpusFixture(t)
	for _, oc := range oracleConfigs {
		t.Run(oc.name, func(t *testing.T) {
			t.Parallel()
			compared, mismatches := 0, 0
			for _, e := range entries {
				// Rebuilt per subtest: the parallel subtests must not share
				// candidate slices with each other or with the shuffling test.
				cands := e.Cands()
				for _, c := range cands {
					compared++
					want := verifyWithAsOfMain(oc.cfg, cands, c.Scene.ID, c.Scene.Title, e.Release)
					got := VerifyWith(oc.cfg, cands, c.Scene.ID, c.Scene.Title, e.Release)
					ok := want.Verified == got.Verified && want.Confidence == got.Confidence
					exp := VerifyResult{Verified: got.Verified, Confidence: got.Confidence}
					if oc.explain {
						ex := ExplainVerifyWith(oc.cfg, cands, c.Scene.ID, c.Scene.Title, e.Release)
						exp = VerifyResult{Verified: ex.Verified, Confidence: ex.Confidence}
						ok = ok && want.Verified == exp.Verified && want.Confidence == exp.Confidence
					}
					if ok {
						continue
					}
					mismatches++
					if mismatches <= 5 {
						t.Errorf("release %q scene %q: pre-refactor says (%v, %.6f), VerifyWith says (%v, %.6f), Explain says (%v, %.6f)",
							e.Release, c.Scene.ID,
							want.Verified, want.Confidence,
							got.Verified, got.Confidence,
							exp.Verified, exp.Confidence)
					}
				}
			}
			if compared < 10000 {
				t.Fatalf("only %d comparisons: the fixture is not being replayed in full, so a green result means nothing", compared)
			}
			if mismatches > 0 {
				t.Errorf("%d of %d verdicts differ from the pre-refactor verifier", mismatches, compared)
			}
			t.Logf("%d verdicts compared against the pre-refactor verifier, %d differ", compared, mismatches)
		})
	}
}

// TestVerifyMatchesPreRefactorOracleShuffled re-runs the comparison with the
// candidate order permuted.
//
// Order is load-bearing in ways that are easy to break silently: isTop reads
// position 0, the rival scan breaks at the first owning title, and runner-up
// confidence is a max over the rest. The recorded corpus is always sorted by
// confidence descending, so replaying it as recorded never puts a low scorer
// first and never asks what happens when the target is not at the front.
// Shuffling produces candidate sets the matcher would not itself emit, which is
// the point: it is the oracle that says what the answer should be, not the
// realism of the input.
func TestVerifyMatchesPreRefactorOracleShuffled(t *testing.T) {
	entries := loadCorpusFixture(t)
	// Fixed seed: a failure has to be reproducible, and a test that shuffles
	// differently each run reports a bug that the next run cannot find.
	rng := rand.New(rand.NewSource(20260802))
	compared, mismatches := 0, 0
	for _, e := range entries {
		cands := e.Cands()
		rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
		for _, c := range cands {
			compared++
			want := verifyWithAsOfMain(DefaultVerifyConfig, cands, c.Scene.ID, c.Scene.Title, e.Release)
			got := VerifyWith(DefaultVerifyConfig, cands, c.Scene.ID, c.Scene.Title, e.Release)
			if want.Verified == got.Verified && want.Confidence == got.Confidence {
				continue
			}
			mismatches++
			if mismatches <= 5 {
				t.Errorf("shuffled release %q scene %q: pre-refactor (%v, %.6f), shipped (%v, %.6f)",
					e.Release, c.Scene.ID, want.Verified, want.Confidence, got.Verified, got.Confidence)
			}
		}
	}
	if mismatches > 0 {
		t.Errorf("%d of %d shuffled verdicts differ from the pre-refactor verifier", mismatches, compared)
	}
	t.Logf("%d shuffled verdicts compared, %d differ", compared, mismatches)
}

// TestOracleCatchesATranscriptionError is the test-of-the-test.
//
// TestExplainAgreesWithVerifyOnCorpus was believed to prove faithfulness and
// does not, and nothing in the tree demonstrated the difference. This injects
// the defect class the oracle is claimed to catch, one gate condition carrying
// the wrong threshold, and asserts the oracle sees it while the
// verdict-vs-explanation comparison does not. If someone later weakens the
// oracle into another comparison of the new code with itself, this fails.
//
// The defect is modelled by feeding the shipped verifier a config in which
// RankMinTitleOverlap has been replaced by StrongTitleOverlap, while the oracle
// keeps the real one. RankMinTitleOverlap (0.15) and StrongTitleOverlap (0.40)
// are both "a title overlap threshold", they sit four lines apart in
// VerifyConfig, and both appear in the rank-title gate, so reaching for the
// wrong one is the realistic mistake, not a contrived one. Evaluating the two
// implementations under different thresholds is the same observable situation
// as one of them having the constant wrong in its source.
//
// WHAT THIS DOES NOT CLAIM: that the oracle catches every transcription error.
// Measured on this corpus, swapping StrongMatchConf (0.70) for DateAnchorMinConf
// (0.65) changes ZERO verdicts, and so does swapping ShortTitleMinConf for
// TitleMinConf: no recorded candidate lands in the affected band with the rest
// of its gate satisfied. The oracle catches errors that change an answer on the
// corpus. Errors in a band the corpus does not populate are invisible to it,
// and to every other test here.
func TestOracleCatchesATranscriptionError(t *testing.T) {
	entries := loadCorpusFixture(t)

	wrong := DefaultVerifyConfig
	wrong.RankMinTitleOverlap = DefaultVerifyConfig.StrongTitleOverlap
	if wrong.RankMinTitleOverlap == DefaultVerifyConfig.RankMinTitleOverlap {
		t.Fatal("the two thresholds are now equal, so this no longer injects anything")
	}

	// Enough evidence to prove sensitivity, then stop. A full replay here
	// would triple this file's CI cost to raise the count and change nothing
	// about the conclusion.
	const enough = 25
	oracleSaw, modeAgreementSaw, compared := 0, 0, 0
	for _, e := range entries {
		if oracleSaw >= enough {
			break
		}
		cands := e.Cands()
		for _, c := range cands {
			compared++
			// Oracle comparison: correct rules vs. the mistranscribed rules.
			ref := verifyWithAsOfMain(DefaultVerifyConfig, cands, c.Scene.ID, c.Scene.Title, e.Release)
			bad := VerifyWith(wrong, cands, c.Scene.ID, c.Scene.Title, e.Release)
			if ref.Verified != bad.Verified {
				oracleSaw++
			}
			// What TestExplainAgreesWithVerifyOnCorpus does: both of its arms
			// carry the same mistake, so they still agree with each other.
			badExplained := ExplainVerifyWith(wrong, cands, c.Scene.ID, c.Scene.Title, e.Release)
			if bad.Verified != badExplained.Verified || bad.Confidence != badExplained.Confidence {
				modeAgreementSaw++
			}
		}
	}
	if oracleSaw == 0 {
		t.Errorf("the oracle comparison did not notice a changed gate threshold over %d verdicts, so it proves nothing", compared)
	}
	if modeAgreementSaw != 0 {
		t.Errorf("verdict/explanation modes disagreed (%d times) under the injected defect; "+
			"that is a separate bug, but it also means this test is no longer demonstrating what it claims", modeAgreementSaw)
	}
	t.Logf("injected threshold error over %d verdicts: oracle saw %d changed, verdict-vs-explanation agreement saw %d",
		compared, oracleSaw, modeAgreementSaw)
}
