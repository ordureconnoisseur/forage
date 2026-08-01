package matcher

import "testing"

// A BTS cut shares its main scene's cast, date, studio and title, so every
// weighted signal ties and the ranking between them is arbitrary. The release
// naming one of the two is the only discriminating fact there is.
func TestRefuseBehindTheScenes(t *testing.T) {
	main := cand("main", "Kenzie Reeves & Lexi Lore: Oily Anal", "2020-06-07", 0.82, 0.45, "Kenzie Reeves", "Lexi Lore")
	bts := cand("bts", "BTS-Kenzie Reeves & Lexi Lore: Oily Anal", "2020-06-07", 0.81, 0.44, "Kenzie Reeves", "Lexi Lore")
	cands := []Candidate{main, bts}
	rel := "EvilAngel.20.06.07.Kenzie.Reeves.And.Lexi.Lore.Oily.Anal.XXX.1080p"

	if VerifyWith(DefaultVerifyConfig, cands, "bts", bts.Scene.Title, rel).Verified {
		t.Error("a BTS cut must not verify against a release that does not name one")
	}
	if !VerifyWith(DefaultVerifyConfig, cands, "main", main.Scene.Title, rel).Verified {
		t.Error("the main scene must still verify")
	}
}

// A release that DOES ask for the BTS cut still gets it. The rule is about
// the release naming one of the two, not about BTS being unwanted.
func TestBehindTheScenesStillVerifiesWhenAskedFor(t *testing.T) {
	bts := cand("bts", "BTS-Attorney Privileges", "2018-09-23", 0.85, 0.5, "Chloe Cherry", "Brandi Love")
	cands := []Candidate{bts}
	for _, rel := range []string{
		"SweetheartVideo.18.09.23.Chloe.Cherry.And.Brandi.Love.Attorney.Privileges.BTS.XXX.1080p",
		"Sweetheart - Attorney Privileges - Behind The Scenes [1080p]",
		"LoverHerFeet.com.Bonus_.Behind.the.Scenes.Chloe.Cherry.1080p",
	} {
		if !VerifyWith(DefaultVerifyConfig, cands, "bts", bts.Scene.Title, rel).Verified {
			t.Errorf("release names a BTS cut and must still verify: %s", rel)
		}
	}
}

func TestLooksBehindTheScenes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"BTS-Kenzie Reeves & Lexi Lore: Oily Anal", true},
		{"Kenzie Reeves BTS", true},
		{"Chloe Cherry (Behind The Scenes)", true},
		{"Behind.the.Scenes.Chloe.Cherry", true},
		{"Behind_The_Scenes", true},
		{"Attorney Privileges", false},
		// Must not fire on a word that merely contains the letters.
		{"Debtsettlement", false},
		{"Her Debts", false},
		{"", false},
	} {
		if got := looksBehindTheScenes(c.in); got != c.want {
			t.Errorf("looksBehindTheScenes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
