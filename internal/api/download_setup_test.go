package api

import (
	"strings"
	"testing"

	"github.com/ordureconnoisseur/forager/internal/placer"
)

// A staging disk is a LAYOUT, not a fault. The old wording told anyone using
// the common "download to a fast local SSD, then move to the NAS" setup to
// "put both on one mount", which is advice to undo something they chose on
// purpose, repeated on every visit to setup.
func TestCrossDeviceAdviceDoesNotTellUsersToUndoTheirLayout(t *testing.T) {
	res := placer.HardlinkResult{OK: true, Hardlink: false, CrossDevice: true,
		Reason: "download folder and library are on different filesystems, so placement copies instead of hardlinking"}

	for _, banned := range []string{"one mount", "double the space)"} {
		if strings.Contains(res.Reason, banned) {
			t.Errorf("probe reason still prescribes a fix (%q): %s", banned, res.Reason)
		}
	}
	if !res.CrossDevice {
		t.Error("cross-device must be distinguishable from a real link failure")
	}
	// The distinction that matters downstream: cross-device still WORKS.
	if !res.OK {
		t.Error("a copy fallback is a working placement, not an error")
	}
}

// Every other link failure is a genuine fault and must stay one, so marking
// cross-device as acceptable cannot quietly excuse an unwritable library.
func TestNonCrossDeviceFailureIsStillAFailure(t *testing.T) {
	res := placer.HardlinkResult{Reason: "couldn't link into the library: permission denied"}
	if res.OK || res.CrossDevice {
		t.Error("a permissions failure is neither OK nor cross-device")
	}
}
