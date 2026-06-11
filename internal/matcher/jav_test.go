package matcher

import (
	"reflect"
	"testing"
)

func TestExtractJAVCodes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// Canonical forms keep extracting.
		{"SNOS-233", []string{"snos-233"}},
		{"SNOS.233 1080p", []string{"snos-233"}},
		{"OAE-302ch", []string{"oae-302"}},
		{"326FCT-221", []string{"fct-221", "326fct-221"}},
		{"FC2-PPV-1234567", []string{"ppv-1234567"}},
		// Resolution-sized digits alone must NOT disqualify a real label.
		{"ABP-1080", []string{"abp-1080"}},
		// A glued part-suffix starts with 'p' but the digits aren't a
		// resolution — the code must keep extracting.
		{"IPX-602part2.mkv", []string{"ipx-602"}},
		{"SNOS-233Part1", []string{"snos-233"}},

		// Quality/codec markers must not mint pseudo-codes: a shared code is
		// scene IDENTITY downstream (javCodeFloor + release verification), so
		// "XXX.1080p" on both sides used to verify unrelated releases.
		{"Angela.White.Title.XXX.1080p.MP4-GRP", nil},
		{"Some.Title.WEB-DL.1080p.x264", nil},
		{"Other.Title.FHD-1080", nil},
		{"Clip.XVID.480p", nil},
		{"Show.HDTV.720p", nil},
		// p/i straight after the digits is a resolution token even for an
		// unlisted letter block.
		{"GRP.2160p", nil},
	}
	for _, c := range cases {
		got := ExtractJAVCodes(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExtractJAVCodes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
