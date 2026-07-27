package engine

import (
	"context"

	"github.com/anacrolix/torrent"
)

// Quiet strips networking down for tests (no DHT/trackers, random port).
func (e *Engine) Quiet() { e.quiet = true }

// ClientForTest exposes the live session so a test can wire two engines
// together as direct peers.
func (e *Engine) ClientForTest() *torrent.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cl
}

// AccountForTest runs one accounting pass.
func (e *Engine) AccountForTest() { e.account(context.Background()) }
