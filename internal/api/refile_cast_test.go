package api

import "testing"

// The folder name comes from forage's own record and the cast from StashDB,
// so they name the same person without always spelling them identically. A
// false negative here re-files a scene into a second folder for someone who
// already has one.
func TestSameFolderName(t *testing.T) {
	same := [][2]string{
		{"Harley Love", "harley love"},
		{"Scarlett Rosewood", "Scarlett  Rosewood"},
		{"Alex Grey", "Alex-Grey"},
		{"J. Doe", "JDoe"},
	}
	for _, c := range same {
		if !sameFolderName(c[0], c[1]) {
			t.Errorf("sameFolderName(%q, %q) = false, want true", c[0], c[1])
		}
	}
	diff := [][2]string{
		{"Harley Love", "Harley Lovee"},
		{"Gigi Dior", "Kianna Dior"},
		{"", "Harley Love"},
		{"Harley Love", ""},
	}
	for _, c := range diff {
		if sameFolderName(c[0], c[1]) {
			t.Errorf("sameFolderName(%q, %q) = true, want false", c[0], c[1])
		}
	}
}
