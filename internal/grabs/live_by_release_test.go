package grabs

import (
	"context"
	"testing"
)

// The release that motivated rule 3: forage took this from NZBgeek, then 14
// days later re-downloaded the identical file from NZBFinder, because the
// (title, indexer) identity treats a cross-posted release as two releases.
const crossPostTitle = "Bang.YNGR.26.07.10.Liora.Vane.XXX.1080p.MP4-WRB"

// TestLiveByReleaseCrossPostBlockedOnceLanded is the waste case: the same
// release title from a DIFFERENT indexer must be recognised once the earlier
// grab's file is on disk. Re-downloading a file you already hold gains
// nothing.
func TestLiveByReleaseCrossPostBlockedOnceLanded(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	if _, err := r.Insert(ctx, Grab{
		ReleaseTitle: crossPostTitle, ReleaseIndexer: "NZBgeek",
		DownloadURL: "http://prowlarr/2/download?link=OLD",
		Client:      "sabnzbd", Status: "confirmed", GrabbedAt: 100,
		PlacedPath: "/lib/Liora Vane/Bang.YNGR.26.07.10.Liora.Vane.XXX.1080p.MP4-WRB.mp4",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A fresh offer of the same release: new URL, different indexer.
	got, err := r.LiveByRelease(ctx, "http://prowlarr/1/download?link=NEW", crossPostTitle, "NZBFinder")
	if err != nil {
		t.Fatalf("LiveByRelease: %v", err)
	}
	if got == nil {
		t.Fatal("cross-posted release with the file already on disk was not recognised")
	}
	if got.ReleaseIndexer != "NZBgeek" {
		t.Fatalf("matched indexer %q, want the landed NZBgeek grab", got.ReleaseIndexer)
	}
}

// TestLiveByReleaseCrossPostAllowsRecovery is the property that makes rule 3
// safe, and the reason it is gated on placed_path instead of simply dropping
// the indexer: "same title, different indexer" is ALSO the standard recovery
// move when one indexer's Usenet post is incomplete or unrepairable. An
// earlier attempt that never landed must never block a second source.
func TestLiveByReleaseCrossPostAllowsRecovery(t *testing.T) {
	ctx := context.Background()

	// Every state an attempt can sit in with nothing on disk yet.
	for _, status := range []string{"queued", "downloading", "completed", "deferred"} {
		t.Run(status, func(t *testing.T) {
			r := newTestRepo(t)
			if _, err := r.Insert(ctx, Grab{
				ReleaseTitle: crossPostTitle, ReleaseIndexer: "NZBgeek",
				DownloadURL: "http://prowlarr/2/download?link=OLD",
				Client:      "sabnzbd", Status: status, GrabbedAt: 100,
				// no PlacedPath — nothing landed
			}); err != nil {
				t.Fatalf("insert: %v", err)
			}
			got, err := r.LiveByRelease(ctx, "http://prowlarr/1/download?link=NEW", crossPostTitle, "NZBFinder")
			if err != nil {
				t.Fatalf("LiveByRelease: %v", err)
			}
			if got != nil {
				t.Fatalf("a %s attempt with no placed file blocked a different indexer's copy; "+
					"that is the recovery path for a dead Usenet post", status)
			}
		})
	}
}

// TestLiveByReleaseSameIndexerUnchanged pins rule 2: within one indexer the
// title is enough on its own, landed or not, so Prowlarr's rotating URLs
// can't re-offer a release forever. Rule 3 must not have narrowed this.
func TestLiveByReleaseSameIndexerUnchanged(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	if _, err := r.Insert(ctx, Grab{
		ReleaseTitle: crossPostTitle, ReleaseIndexer: "NZBgeek",
		DownloadURL: "http://prowlarr/2/download?link=OLD",
		Client:      "sabnzbd", Status: "downloading", GrabbedAt: 100,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := r.LiveByRelease(ctx, "http://prowlarr/2/download?link=ROTATED", crossPostTitle, "NZBgeek")
	if err != nil {
		t.Fatalf("LiveByRelease: %v", err)
	}
	if got == nil {
		t.Fatal("same indexer + same title should still match through a rotated URL")
	}
}

// TestLiveByReleaseFailedNeverBlocks: a failed attempt is not an obstacle to
// re-grabbing, whichever identity would otherwise match. Pre-existing
// behaviour, pinned because rule 3 touches the same WHERE clause.
func TestLiveByReleaseFailedNeverBlocks(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	if _, err := r.Insert(ctx, Grab{
		ReleaseTitle: crossPostTitle, ReleaseIndexer: "NZBgeek",
		DownloadURL: "http://prowlarr/2/download?link=OLD",
		Client:      "sabnzbd", Status: "failed", GrabbedAt: 100,
		// even with a placed file, a failed grab must not block
		PlacedPath: "/lib/Liora Vane/old.mp4",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, indexer := range []string{"NZBgeek", "NZBFinder"} {
		got, err := r.LiveByRelease(ctx, "http://prowlarr/2/download?link=OLD", crossPostTitle, indexer)
		if err != nil {
			t.Fatalf("LiveByRelease: %v", err)
		}
		if got != nil {
			t.Fatalf("failed grab blocked a re-grab from %s", indexer)
		}
	}
}

// TestLiveByReleaseEmptyTitleMatchesNothing guards the widened clause against
// the degenerate case: adopted grabs and hand-added files can carry an empty
// title, and an empty-matches-empty rule would make one of them block every
// future grab that also lacks a title.
func TestLiveByReleaseEmptyTitleMatchesNothing(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	if _, err := r.Insert(ctx, Grab{
		ReleaseTitle: "", ReleaseIndexer: "", Client: "qbit", ClientID: "abc",
		Status: "confirmed", GrabbedAt: 100, PlacedPath: "/lib/Someone/file.mp4",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := r.LiveByRelease(ctx, "", "", "")
	if err != nil {
		t.Fatalf("LiveByRelease: %v", err)
	}
	if got != nil {
		t.Fatalf("empty title matched grab %d; an untitled grab must not block anything", got.ID)
	}
}

// TestLiveByReleaseDifferentTitleNoMatch: the sanity floor. A landed grab must
// not shadow an unrelated release.
func TestLiveByReleaseDifferentTitleNoMatch(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	if _, err := r.Insert(ctx, Grab{
		ReleaseTitle: crossPostTitle, ReleaseIndexer: "NZBgeek",
		Client: "sabnzbd", Status: "confirmed", GrabbedAt: 100,
		PlacedPath: "/lib/Liora Vane/a.mp4",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := r.LiveByRelease(ctx, "http://prowlarr/1/download?link=X",
		"Bang.YNGR.26.08.02.Someone.Else.XXX.1080p.MP4-WRB", "NZBFinder")
	if err != nil {
		t.Fatalf("LiveByRelease: %v", err)
	}
	if got != nil {
		t.Fatalf("unrelated title matched grab %d", got.ID)
	}
}

// TestLiveByReleaseUpgradeStillPossible: an upgrade is a DIFFERENT release
// (different resolution/group, hence a different title) for a scene you
// already hold, so rule 3 must leave the upgrade path alone.
func TestLiveByReleaseUpgradeStillPossible(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	if _, err := r.Insert(ctx, Grab{
		ReleaseTitle: "Bang.YNGR.26.07.10.Liora.Vane.XXX.720p.MP4-WRB", ReleaseIndexer: "NZBgeek",
		Client: "sabnzbd", Status: "confirmed", GrabbedAt: 100,
		PlacedPath: "/lib/Liora Vane/720p.mp4",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// The 1080p cut of the same scene: different title, so not the same release.
	got, err := r.LiveByRelease(ctx, "http://prowlarr/1/download?link=UP", crossPostTitle, "NZBFinder")
	if err != nil {
		t.Fatalf("LiveByRelease: %v", err)
	}
	if got != nil {
		t.Fatalf("a 720p grab blocked the 1080p upgrade (matched grab %d)", got.ID)
	}
}
