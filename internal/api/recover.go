package api

import (
	"fmt"
	"runtime/debug"

	"github.com/ordureconnoisseur/forager/internal/paniclog"
)

// recoverPanic logs a panic from a background goroutine instead of letting
// it kill the daemon. chi's Recoverer covers HTTP handlers only; the job
// workers, watch loop, and config-triggered cache refreshes all chew
// release names and JSON shapes from external services (Stash, StashDB,
// Prowlarr, the download clients) where a nil-optional-field panic must
// fail that one unit of work, not take down the HTTP server, poller, and
// every other loop with it.
//
// The panic is also persisted (paniclog) so a recurring crash in a
// nightly loop shows up in /healthz and the diagnostics bundle instead of
// only in logs nobody reads.
//
//	go func() {
//		defer s.recoverPanic("label")
//		...
//	}()
func (s *Server) recoverPanic(label string) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		s.log.Error("panic in background goroutine",
			"in", label, "panic", r, "stack", string(stack))
		paniclog.Record(s.db, label, fmt.Sprint(r), stack)
	}
}
