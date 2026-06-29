package prowlarr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestToReleaseGrabURL pins how a grab link is derived across indexer shapes —
// especially aggregators (Knaben) that expose it ONLY as an http magnetUrl
// proxy (which 301s to a magnet), not a downloadUrl or raw magnet:. That proxy
// must become the DownloadURL or the release looks grabbable but isn't.
func TestToReleaseGrabURL(t *testing.T) {
	cases := []struct {
		name        string
		raw         rawRelease
		wantGrabURL string
	}{
		{"plain downloadUrl", rawRelease{DownloadURL: "http://x/a.torrent"}, "http://x/a.torrent"},
		{"raw magnet in magnetUrl", rawRelease{MagnetURL: "magnet:?xt=urn:btih:abc"}, "magnet:?xt=urn:btih:abc"},
		{"raw magnet in guid", rawRelease{GUID: "magnet:?xt=urn:btih:def"}, "magnet:?xt=urn:btih:def"},
		{"Knaben proxy magnetUrl (http)", rawRelease{MagnetURL: "http://prowlarr/16/download?link=z"}, "http://prowlarr/16/download?link=z"},
		{"downloadUrl wins over proxy", rawRelease{DownloadURL: "http://x/a.torrent", MagnetURL: "http://prowlarr/16/download?link=z"}, "http://x/a.torrent"},
		{"nothing grabbable", rawRelease{GUID: "https://site/description?id=1"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.raw.toRelease().GrabURL(); got != c.wantGrabURL {
				t.Errorf("GrabURL = %q, want %q", got, c.wantGrabURL)
			}
		})
	}
}

// TestRecentReleasesQueryless pins the RSS-feed behaviour: RecentReleases must
// hit /api/v1/search with an EMPTY query (the query-less recent feed), pass the
// categories through, and parse publishDate/indexerId onto the Release.
func TestRecentReleasesQueryless(t *testing.T) {
	var gotQuery string
	var gotQueryPresent bool
	var gotCats []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery = q.Get("query")
		_, gotQueryPresent = q["query"]
		gotCats = q["categories"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"title":"Studio.Performer.XXX.1080p","indexer":"NZBgeek","indexerId":2,"protocol":"usenet","size":123,"grabs":5,"publishDate":"2026-06-28T00:24:31Z","infoUrl":"http://x/1","downloadUrl":"http://x/1.nzb"}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	rels, err := c.RecentReleases(context.Background(), []int{6000, 6050})
	if err != nil {
		t.Fatal(err)
	}
	// Query param is sent but empty — Prowlarr's recent-feed trigger.
	if !gotQueryPresent {
		t.Error("query param missing; recent feed needs an explicit empty query=")
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty (recent feed must not send a keyword)", gotQuery)
	}
	if len(gotCats) != 2 || gotCats[0] != "6000" || gotCats[1] != "6050" {
		t.Errorf("categories = %v, want [6000 6050]", gotCats)
	}
	if len(rels) != 1 {
		t.Fatalf("releases = %d, want 1", len(rels))
	}
	if rels[0].PublishDate != "2026-06-28T00:24:31Z" {
		t.Errorf("PublishDate = %q, not parsed", rels[0].PublishDate)
	}
	if rels[0].IndexerID != 2 {
		t.Errorf("IndexerID = %d, want 2", rels[0].IndexerID)
	}
}
