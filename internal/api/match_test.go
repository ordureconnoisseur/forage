package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

func TestGrabMatchName(t *testing.T) {
	cases := []struct {
		name string
		g    grabs.Grab
		want string
	}{
		{"release title wins", grabs.Grab{ReleaseTitle: "Some Release", PlacedPath: "/x/B.mp4", ClientName: "C"}, "Some Release"},
		{"placed basename when no title", grabs.Grab{PlacedPath: "/data/porn/Media/Unsorted/BLACKED_107066_1080P.mp4", ClientName: "C"}, "BLACKED_107066_1080P.mp4"},
		{"client name as last resort", grabs.Grab{ClientName: "torrent name"}, "torrent name"},
		{"empty when nothing", grabs.Grab{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grabMatchName(&tc.g); got != tc.want {
				t.Fatalf("grabMatchName = %q, want %q", got, tc.want)
			}
		})
	}
}
