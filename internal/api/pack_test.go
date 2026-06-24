package api

import "testing"

func TestIsPackRelease(t *testing.T) {
	packs := []string{
		"Cyber Doll aka Gia Venetia MegaPACK",
		"Gia Venetia Mega Pack",
		"Gia Venetia mega-pack 2026",
		"Sweetie Fox PACK [2020-2026]",
		"Evil Angel Collection",
		"Abella Danger 50 Scenes SiteRip",
		"Performer - 120 videos",
	}
	for _, p := range packs {
		if !isPackRelease(p) {
			t.Errorf("isPackRelease(%q) = false, want true (it's a pack)", p)
		}
	}

	singles := []string{
		"BlacksOnBlondes.26.06.19.Cyber.Doll.XXX.1080p.MP4-WRB",
		"[ManyVids.com] Sweetie Fox - Busty Stranger on a Bicycle [2026-06-12, Anal, 1080p, SiteRip]",
		"VRSpy - Pass-Through Cyber Doll On Demand 8K",
		"Wild Open House XXX 2160p",
		"OAE302",
	}
	for _, s := range singles {
		if isPackRelease(s) {
			t.Errorf("isPackRelease(%q) = true, want false (single scene)", s)
		}
	}
}
