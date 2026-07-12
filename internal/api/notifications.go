package api

import (
	"net/http"
	"time"
)

// notificationCounts are the actionable, current-attention signals the
// header bell + Watching-tab badge surface. Deliberately NOT "new scenes"
// — discovery is browse-when-you-like (the Discover tab), not an alert.
type notificationCounts struct {
	WatchesAvailable  int `json:"watches_available"`   // watches that found a release, ready to grab
	GrabsStalled      int `json:"grabs_stalled"`       // downloads stuck with no progress
	GrabsPlaceFailing int `json:"grabs_place_failing"` // downloaded but can't get into the library
	GrabsFailed       int `json:"grabs_failed"`        // recently-failed grabs (not all-time)
}

// failedNotifyWindow bounds the "failed" count to recent failures — an
// old failed grab from last week isn't something to nag about.
const failedNotifyWindow = 7 * 24 * time.Hour

func (s *Server) getNotifications(w http.ResponseWriter, r *http.Request) {
	var out notificationCounts
	if s.watches != nil {
		if n, err := s.watches.CountAvailable(r.Context()); err == nil {
			out.WatchesAvailable = n
		}
	}
	if s.grabs != nil {
		since := time.Now().Add(-failedNotifyWindow).Unix()
		if n, err := s.grabs.CountRecentFailed(r.Context(), since); err == nil {
			out.GrabsFailed = n
		}
		// Stalled is a computed (not stored) flag, so count it by applying
		// the same isStalled check the /grabs view uses over the currently
		// downloading grabs.
		if dl, err := s.grabs.List(r.Context(), "downloading", "", 200, 0); err == nil {
			for _, g := range dl {
				if isStalled(g) {
					out.GrabsStalled++
				}
			}
		}
		// Placement-failing: downloaded grabs stuck at "completed" with a
		// persistent place_error.
		if comp, err := s.grabs.List(r.Context(), "completed", "", 200, 0); err == nil {
			for _, g := range comp {
				if isPlaceFailing(g) {
					out.GrabsPlaceFailing++
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
