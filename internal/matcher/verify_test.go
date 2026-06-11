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

// TestVerifyStrongMatchNotBlockedByCastNamedRival: the rival-title guard
// must ignore overlap that comes purely from the rival's own cast names.
// Solo/intro catalog scenes are titled after their performer ("Paris
// Lincoln Solo 4"), so they "match" any release naming that performer —
// cast coincidence, not the title discriminating between siblings. This
// systematically blocked the strong-match path for the dominant
// title-less release shape (site.date.performer.mp4) whose true scene
// sits at #1 with an exact date. Mirrors corpus case
// gotfilled.26.03.20.paris.lincoln.mp4.
func TestVerifyStrongMatchNotBlockedByCastNamedRival(t *testing.T) {
	expected := "the-scene"
	cands := []Candidate{
		{
			Scene: stashdb.Scene{ID: expected, Title: "In The Ass Of Paris", Date: "2026-03-20",
				Performers: []stashdb.ScenePerformer{{Name: "Paris Lincoln"}, {Name: "Romeo Mancini"}}},
			Confidence:   0.72, // performer + exact date + cast agree
			TitleOverlap: 0.08,
		},
		{
			Scene: stashdb.Scene{ID: "solo-4", Title: "Paris Lincoln Solo 4",
				Performers: []stashdb.ScenePerformer{{Name: "Paris Lincoln"}}},
			Confidence:   0.57,
			TitleOverlap: 0.33, // inflated purely by the cast name
		},
	}
	release := "gotfilled.26.03.20.paris.lincoln.mp4"
	vr := Verify(cands, expected, "In The Ass Of Paris", release)
	if !vr.Verified {
		t.Errorf("cast-name-only rival overlap must not block the strong-match path")
	}
	// And the rival itself must still not verify.
	if Verify(cands, "solo-4", "Paris Lincoln Solo 4", release).Verified {
		t.Errorf("the cast-named rival must not verify")
	}
}

// TestVerifyDateAnchored covers the date-anchored path for title-less
// releases (site.26.03.11.performer.mp4): below the 0.70 strong-match
// bar such releases were structurally unverifiable (no title → overlap
// and containment can never fire). A unique exact date stated by the
// release discriminates like a title; a same-day sibling removes the
// discrimination and must refuse.
func TestVerifyDateAnchored(t *testing.T) {
	mk := func(id, title, date string, conf, ov float64) Candidate {
		return Candidate{Scene: stashdb.Scene{ID: id, Title: title, Date: date,
			Performers: []stashdb.ScenePerformer{{Name: "Haley Spades"}}},
			Confidence: conf, TitleOverlap: ov}
	}
	release := "freeusefantasy.26.03.11.haley.spades.mp4"

	// Unique exact date at performer+date confidence → verifies.
	cands := []Candidate{
		mk("right", "Using My Stepsis All Day", "2026-03-11", 0.66, 0.05),
		mk("other", "Some Earlier Scene", "2026-02-01", 0.46, 0.05),
	}
	if !Verify(cands, "right", "Using My Stepsis All Day", release).Verified {
		t.Errorf("unique exact-date #1 at 0.66 must verify via the date anchor")
	}

	// A same-day sibling kills the anchor — date can't discriminate.
	cands = []Candidate{
		mk("right", "Using My Stepsis All Day", "2026-03-11", 0.66, 0.05),
		mk("sibling", "BTS Of The Same Day", "2026-03-11", 0.61, 0.05),
	}
	if Verify(cands, "right", "Using My Stepsis All Day", release).Verified {
		t.Errorf("same-day sibling must block the date anchor")
	}

	// Date-disagreeing scene (the ~0.51 performer-only band) stays out:
	// the release's date isn't the scene's date.
	cands = []Candidate{
		mk("drifted", "Late Repost", "2026-03-19", 0.51, 0.05),
	}
	if Verify(cands, "drifted", "Late Repost", release).Verified {
		t.Errorf("a scene whose date the release does not state must not verify")
	}

	// No date in the release at all → anchor can't fire.
	cands = []Candidate{
		mk("right", "Using My Stepsis All Day", "2026-03-11", 0.66, 0.05),
	}
	if Verify(cands, "right", "Using My Stepsis All Day", "CarliSmall").Verified {
		t.Errorf("date anchor must require the release to state the date")
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
