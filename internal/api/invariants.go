package api

import "net/http"

// getInvariants returns the invariant checker's most recent full report:
// every assertion, whether it holds, and for the ones that don't, the rows
// that break them with what is inconsistent about each.
//
// Authenticated, unlike the /healthz digest, because the samples name
// library paths and release titles. The digest answers "is anything wrong";
// this answers "which rows, and what about them".
//
// Read-only, like the checker itself. There is no repair endpoint and no
// dismiss: a violation goes away when the underlying data stops violating,
// which is the only signal worth having.
func (s *Server) getInvariants(w http.ResponseWriter, r *http.Request) {
	if s.invariants == nil {
		writeErr(w, http.StatusServiceUnavailable, "invariant checker not running")
		return
	}
	rep := s.invariants()
	if rep == nil {
		// The first pass runs on the poller's first tick, so this window is
		// seconds long at boot. 503 rather than an empty report: "not
		// measured yet" and "measured, nothing wrong" must not look alike.
		writeErr(w, http.StatusServiceUnavailable, "no invariant report yet; the first pass runs on the next poller tick")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
