package api

import "runtime/debug"

// recoverPanic logs a panic from a background goroutine instead of letting
// it kill the daemon. chi's Recoverer covers HTTP handlers only; the job
// workers, watch loop, and config-triggered cache refreshes all chew
// release names and JSON shapes from external services (Stash, StashDB,
// Prowlarr, the download clients) where a nil-optional-field panic must
// fail that one unit of work, not take down the HTTP server, poller, and
// every other loop with it.
//
//	go func() {
//		defer s.recoverPanic("label")
//		...
//	}()
func (s *Server) recoverPanic(label string) {
	if r := recover(); r != nil {
		s.log.Error("panic in background goroutine",
			"in", label, "panic", r, "stack", string(debug.Stack()))
	}
}
