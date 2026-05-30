package matcher

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// TestVerifyStrongMatchLowTitleOverlap is the regression guard for the
// bug where a release that is unmistakably the scene (performer + studio +
// date all agree, conf 0.87) was dumped in Unverified because its title
// Jaccard was crushed by a long tag list and an episode tag the release
// omitted ("Wrestling Stepmom For Pussy – S13:E6").
func TestVerifyStrongMatchLowTitleOverlap(t *testing.T) {
	sceneID := "the-scene"
	cands := []Candidate{
		{
			Scene:        stashdb.Scene{ID: sceneID, Title: "Wrestling Stepmom For Pussy – S13:E6"},
			Confidence:   0.87,
			TitleOverlap: 0.12, // drowned by tag-soup release name
		},
	}
	release := "[BrattyMILF.com / MomLover.com] Asteria Jade – Wrestling Stepmom For Pussy [2026-03-26, Brunette, Big Ass, Big Cock, Blowjob, Creampie, POV, Tattoos, 1080p, SiteRip]"
	vr := Verify(cands, sceneID, "Wrestling Stepmom For Pussy – S13:E6", release)
	if !vr.Verified {
		t.Errorf("expected verified (conf 0.87, identity-level match), got unverified")
	}
}

// TestVerifyPerformerCoincidenceRejected guards the other side: a release
// that merely shares a performer (no date/studio/title agreement, conf
// ~0.46) must NOT verify via the strong-match path.
func TestVerifyPerformerCoincidenceRejected(t *testing.T) {
	sceneID := "wrong-scene"
	cands := []Candidate{
		{
			Scene:        stashdb.Scene{ID: sceneID, Title: "Some Other Scene Entirely"},
			Confidence:   0.46, // performer-only ceiling
			TitleOverlap: 0.05, // floored — no real title overlap
		},
	}
	release := "Studio - Shared Performer - A Completely Different Title 1080p"
	vr := Verify(cands, sceneID, "Some Other Scene Entirely", release)
	if vr.Verified {
		t.Errorf("performer-only coincidence (conf 0.46) must not verify, but did")
	}
}

// TestVerifyStrongMatchBlockedByTitleRival guards the multi-scene-rip
// case: the same cast+date maps to several StashDB scenes (an episode vs
// its BTS vs a TS-on-TS cut), so a same-cast release scores conf >= 0.70
// against ALL of them. The strong-match path must NOT verify the viewed
// scene when a sibling candidate matches the release title clearly better
// — the title is discriminating and shouldn't be overridden by raw conf.
func TestVerifyStrongMatchBlockedByTitleRival(t *testing.T) {
	viewed := "bts-cut"
	rival := "main-episode"
	// The viewed scene is the matcher's #1 (highest conf — same cast/date
	// max it out) but its title barely overlaps; a sibling scene matches
	// the release title clearly better. The rival-title guard must block.
	cands := []Candidate{
		{Scene: stashdb.Scene{ID: viewed, Title: "BTS — Ariel Demure & Kasey Kei"}, Confidence: 0.95, TitleOverlap: 0.10},
		{Scene: stashdb.Scene{ID: rival, Title: "Take A Ride On The Trans Train"}, Confidence: 0.80, TitleOverlap: 0.55},
	}
	release := "TakeARideOnTheTransTrain04_s04_ArielDemure_KaseyKei3_1080p.mp4"
	vr := Verify(cands, viewed, "BTS — Ariel Demure & Kasey Kei", release)
	if vr.Verified {
		t.Errorf("viewed #1 must not verify when a sibling has clearly higher title overlap")
	}
}

// TestVerifyStrongMatchOnlyForTop confirms the strong-match path only
// applies to the #1 candidate — a high-conf scene ranked below the top
// pick shouldn't verify off conf alone.
func TestVerifyStrongMatchOnlyForTop(t *testing.T) {
	top := "top-scene"
	other := "other-scene"
	cands := []Candidate{
		{Scene: stashdb.Scene{ID: top, Title: "Top Pick"}, Confidence: 0.90, TitleOverlap: 0.5},
		{Scene: stashdb.Scene{ID: other, Title: "Runner Up"}, Confidence: 0.80, TitleOverlap: 0.05},
	}
	// `other` is conf 0.80 (>= strong threshold) but it's NOT #1.
	vr := Verify(cands, other, "Runner Up", "some release with no Runner Up title overlap")
	if vr.Verified {
		t.Errorf("non-top candidate must not verify via strong-match path")
	}
}
