package grabs

import "sync"

// PendingAdds tracks grab IDs whose asynchronous client-add is still in
// flight in this process — queued behind a fetch gate, mid-request, or in a
// rate-limit backoff chain. The api package marks grabs pending; the poller
// consults it before declaring a still-unlinked queued grab dead: a grab
// waiting out a long fetch-gate queue (a bulk batch tail) must not be failed
// on the link timeout while its add is genuinely still coming. After a
// daemon restart the registry is empty by construction — exactly right,
// because the in-flight add died with the process and the timeout SHOULD
// fire.
//
// All methods are safe on a nil receiver (no-op / not pending), so tests and
// callers without a registry need no guards.
type PendingAdds struct {
	m sync.Map // grab id -> struct{}
}

func NewPendingAdds() *PendingAdds { return &PendingAdds{} }

// Start marks a grab's add as in flight.
func (p *PendingAdds) Start(id int64) {
	if p == nil || id == 0 {
		return
	}
	p.m.Store(id, struct{}{})
}

// Done clears the in-flight mark once the add chain terminates (success,
// hard failure, or bail).
func (p *PendingAdds) Done(id int64) {
	if p == nil || id == 0 {
		return
	}
	p.m.Delete(id)
}

// Has reports whether a grab's add is still in flight.
func (p *PendingAdds) Has(id int64) bool {
	if p == nil || id == 0 {
		return false
	}
	_, ok := p.m.Load(id)
	return ok
}
