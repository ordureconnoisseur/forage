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

	// Speculative readings must not anchor. The conventional (TopDate)
	// reading of "05.06.2024" is the EU 2024-06-05; the US alternate
	// 2024-05-06 exists in AllDates for RANKING, but a scene matching
	// only the alternate must not be decisively verified by it.
	cands = []Candidate{
		mk("alt", "Some Scene", "2024-05-06", 0.66, 0.05),
	}
	if Verify(cands, "alt", "Some Scene", "Studio.05.06.2024.Haley.Spades.mp4").Verified {
		t.Errorf("a non-conventional date reading must not anchor")
	}
	// Fused readings never anchor (member ids masquerade as dates).
	cands = []Candidate{
		mk("fused", "Some Scene", "2026-04-18", 0.66, 0.05),
	}
	if Verify(cands, "fused", "Some Scene", "AnalMom260418Haley.Spades.mp4").Verified {
		t.Errorf("a fused date reading must not anchor")
	}
}

// TestVerifyRivalContainmentVeto: when the release spells out ANOTHER
// candidate's distinctive title, the confidence-driven paths must refuse
// the viewed scene — the release is stating which scene it is. This is
// the holdout case "OnlyFans - Elly Clutch, Van Wylde - Afternoon
// Hookup": the true scene's title contains its own cast names, so
// cast-stripping halved its rival OVERLAP below the margin and a wrong
// high-conf rank-1 strong-match-verified past it; its containment claim
// is untouched by stripping and now vetoes.
func TestVerifyRivalContainmentVeto(t *testing.T) {
	wrong := Candidate{
		Scene: stashdb.Scene{ID: "wrong", Title: "Hot Red Head Super Model Dicked Down - Elly Clutch", Date: "2025-05-29",
			Performers: []stashdb.ScenePerformer{{Name: "Elly Clutch"}, {Name: "Van Wylde"}}},
		Confidence: 0.77, TitleOverlap: 0.11,
	}
	right := Candidate{
		Scene: stashdb.Scene{ID: "right", Title: "Afternoon Hookup With Van Wylde", Date: "2026-02-26",
			Performers: []stashdb.ScenePerformer{{Name: "Elly Clutch"}, {Name: "Van Wylde"}}},
		Confidence: 0.62, TitleOverlap: 0.33,
	}
	cands := []Candidate{wrong, right}
	release := "OnlyFans - Elly Clutch, Van Wylde  - Afternoon Hookup 1080p rq.mp4"

	if Verify(cands, "wrong", wrong.Scene.Title, release).Verified {
		t.Errorf("strong-match must refuse when a rival's title is contained by the release")
	}
	if !Verify(cands, "right", right.Scene.Title, release).Verified {
		t.Errorf("the scene whose title the release states must still verify")
	}
}

// TestVerifyCastListRivalDoesNotVeto: the veto judges a rival's claim on
// its CAST-STRIPPED title. A rival whose title IS its cast list is fully
// "contained" by any release naming the shared cast — a six-performer
// gangbang release names everyone — and that must not veto the correct
// strong match (dev corpus case: AccidentalGangbang "Too Many Tops").
func TestVerifyCastListRivalDoesNotVeto(t *testing.T) {
	cast := []stashdb.ScenePerformer{
		{Name: "Khloe Kay"}, {Name: "Jade Venus"}, {Name: "Ariel Demure"},
		{Name: "Kasey Kei"}, {Name: "Lola Morena"}, {Name: "Avery Lust"},
	}
	right := Candidate{
		Scene:      stashdb.Scene{ID: "right", Title: "Too Many Tops!", Date: "2025-05-03", Performers: cast},
		Confidence: 0.78, TitleOverlap: 0.12,
	}
	castTitled := Candidate{
		Scene:      stashdb.Scene{ID: "cast-titled", Title: "Khloe Kay, Jade Venus, Ariel Demure, Kasey Kei & Avery Lust", Performers: cast},
		Confidence: 0.45, TitleOverlap: 0.30,
	}
	cands := []Candidate{right, castTitled}
	release := "AccidentalGangbang.24.05.03.Khloe.Kay.Jade.Venus.Ariel.Demure.Kasey.Kei.Lola.Morena.and.Avery.Lust.Too.Many.Tops.XXX.2160p.MP4-Narcos"
	if !Verify(cands, "right", right.Scene.Title, release).Verified {
		t.Errorf("a cast-list-titled rival must not veto the correct strong match")
	}
}

// TestVerifyGenericTitleNeedsCorroboration guards the "Doggy Anal" class: a
// scene whose title is entirely generic sex vocabulary must NOT verify on
// title overlap alone — a release naming the same acts identifies no specific
// scene. A distinctive short title with the same overlap still verifies, and a
// generic title still verifies when a real overall match corroborates it.
func TestVerifyGenericTitleNeedsCorroboration(t *testing.T) {
	// All-generic title, title overlap only (no performer/date) → must refuse.
	cands := []Candidate{
		{Scene: stashdb.Scene{ID: "doggy", Title: "Doggy Anal"}, Confidence: 0.20, TitleOverlap: 0.67},
	}
	if Verify(cands, "doggy", "Doggy Anal", "ATK Girlfriends - Doggy And Anal 1080p").Verified {
		t.Errorf("an all-generic title must not verify on title overlap alone")
	}

	// Distinctive short title, same overlap, same lack of corroboration →
	// still verifies (the release names it by something specific).
	cands = []Candidate{
		{Scene: stashdb.Scene{ID: "cyber", Title: "Cyber Doll"}, Confidence: 0.20, TitleOverlap: 0.67},
	}
	if !Verify(cands, "cyber", "Cyber Doll", "ATK Girlfriends - Cyber Doll On Demand 1080p").Verified {
		t.Errorf("a distinctive title must still verify on strong title overlap")
	}

	// Generic title BUT a real overall match (conf past the floor) → the
	// corroborated path still verifies; only the title-alone shortcut is gated.
	cands = []Candidate{
		{Scene: stashdb.Scene{ID: "doggy", Title: "Doggy Anal"}, Confidence: 0.55, TitleOverlap: 0.67},
	}
	if !Verify(cands, "doggy", "Doggy Anal", "ATK Girlfriends - Doggy And Anal 1080p").Verified {
		t.Errorf("a generic title with real corroboration (conf 0.55) must still verify")
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
