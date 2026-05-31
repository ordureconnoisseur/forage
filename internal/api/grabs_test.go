package api

import (
	"testing"
	"time"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

func TestIsStalled(t *testing.T) {
	now := time.Now().Unix()
	ago := func(d time.Duration) int64 { return now - int64(d.Seconds()) }

	cases := []struct {
		name string
		g    grabs.Grab
		want bool
	}{
		{
			"downloading, no progress for 40m → stalled",
			grabs.Grab{Client: "qbit", Status: "downloading", Progress: 0.4, ProgressAt: ago(40 * time.Minute)},
			true,
		},
		{
			"downloading, progressed 5m ago → not stalled",
			grabs.Grab{Client: "qbit", Status: "downloading", Progress: 0.4, ProgressAt: ago(5 * time.Minute)},
			false,
		},
		{
			"never progressed, grabbed 40m ago → stalled (falls back to grab time)",
			grabs.Grab{Client: "qbit", Status: "downloading", Progress: 0, ProgressAt: 0, GrabbedAt: ago(40 * time.Minute)},
			true,
		},
		{
			"never progressed, just grabbed → not stalled",
			grabs.Grab{Client: "qbit", Status: "downloading", Progress: 0, ProgressAt: 0, GrabbedAt: ago(2 * time.Minute)},
			false,
		},
		{
			"complete download is never stalled",
			grabs.Grab{Client: "qbit", Status: "downloading", Progress: 1, ProgressAt: ago(40 * time.Minute)},
			false,
		},
		{
			"only downloading grabs can stall (completed)",
			grabs.Grab{Client: "qbit", Status: "completed", Progress: 0.4, ProgressAt: ago(40 * time.Minute)},
			false,
		},
		{
			"SAB grabs don't carry progress → never stalled",
			grabs.Grab{Client: "sabnzbd", Status: "downloading", Progress: 0, ProgressAt: 0, GrabbedAt: ago(40 * time.Minute)},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isStalled(c.g); got != c.want {
				t.Errorf("isStalled = %v, want %v", got, c.want)
			}
		})
	}
}
