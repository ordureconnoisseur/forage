package api

import (
	"testing"

	"github.com/ordureconnoisseur/forager/internal/grabs"
)

// "refused: " is a generic phrase, and matching it with Contains collided with
// two real production errors that carry it mid-string. Both are infrastructure
// failures the same release survives on a retry, so treating them as
// content-dead meant a wrong qBit password could quietly mark every watch it
// touched as unfixable.
func TestContentDeadReasonRefusalIsAPrefixNotASubstring(t *testing.T) {
	dead := []string{
		grabs.RefusedPrefix + "the release contains no video",
		grabs.RefusedPrefix + "the download is a single .rar file",
	}
	for _, r := range dead {
		if !contentDeadReason(r) {
			t.Errorf("forage's own refusal must be content-dead: %q", r)
		}
	}

	// The exact shapes internal/qbit/client.go and internal/sabnzbd/client.go
	// produce. Infrastructure, not content: retrying is the correct response.
	alive := []string{
		"torrent add: qbit login refused: Fails. (unauthorized)",
		"sab addurl refused: API key incorrect",
		"connection refused: dial tcp 127.0.0.1:8080",
	}
	for _, r := range alive {
		if contentDeadReason(r) {
			t.Errorf("an infrastructure failure must stay retryable: %q", r)
		}
	}
}

// The other markers are still substring matches, because SAB embeds them in
// longer sentences. This guards against the prefix fix over-tightening them.
func TestContentDeadReasonKeepsSubstringMarkers(t *testing.T) {
	for _, r := range []string{
		"Download failed: repair failed, 3 blocks short",
		"job cannot be completed, articles missing",
		"grab gave up after 4 attempts across 2 indexers",
		"qbit declined this torrent (invalid encoding)",
	} {
		if !contentDeadReason(r) {
			t.Errorf("still expected content-dead: %q", r)
		}
	}
}
