package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/pathmap"
	"github.com/ordureconnoisseur/forager/internal/stash"
)

// The Unfiled view: library scenes that are not under a performer folder.
//
// Deliberately NOT built on the grabs table. Filing state is a property of the
// library, and forage's records cover only what it fetched or adopted: 560
// rows against 5,325 scenes under Unsorted on the reference library, and zero
// of the 194 files that were lying loose in the library root. Every count
// produced from grabs was a confident fraction of the problem.
//
// Three buckets, because the work each implies is different:
//
//   - filable: Stash names a performer, so this can be filed right now. Should
//     be empty. The poller files these at confirm time, so anything here is
//     either pre-dating that or a bug, and the invariant checker says so.
//   - identified: a metadata source knows the scene but no performer is
//     attached. Actionable, and finite: the fix is a performer in Stash.
//   - unknown: nothing has ever identified it. For a library that is largely
//     amateur and OnlyFans content this bucket is PERMANENT, not pending.
//
// That last point drives the UI contract: this endpoint reports counts and a
// list, and nothing here should be rendered as a badge or an alert. A number
// that can never reach zero, presented as work, gets ignored, and takes the
// two counts beside it that ARE actionable down with it.

type unfiledScene struct {
	SceneID string `json:"scene_id"`
	Title   string `json:"title"`
	// Path is Stash's view of the file. The UI shows it; the filing action
	// re-derives forage's own view from it.
	Path string `json:"path"`
	// Bucket is one of "filable", "identified", "unknown".
	Bucket string `json:"bucket"`
	// Suggested is the performer this would be filed under, when Stash names
	// one. Empty for the other two buckets.
	Suggested string `json:"suggested,omitempty"`
	// Performers is every performer Stash has on the scene, best first.
	Performers []string `json:"performers,omitempty"`
	Identified bool     `json:"identified"`
}

type unfiledResponse struct {
	Scenes []unfiledScene `json:"scenes"`
	Counts map[string]int `json:"counts"`
	// LibraryRoot is echoed so the UI can render paths relative to it rather
	// than as absolute strings nobody can read.
	LibraryRoot string `json:"library_root"`
}

// bucketFor classifies a scene. Order matters: a scene with a performer is
// filable whether or not anything identified it, because the performer is the
// only thing filing needs.
func bucketFor(s stash.UnfiledScene) string {
	if topPerformerName(s.Performers) != "" {
		return "filable"
	}
	if len(s.StashIDs) > 0 {
		return "identified"
	}
	return "unknown"
}

// topPerformerName picks the performer whose folder the file belongs in: the
// one already holding the most scenes. Same rule as the poller's re-file and
// the pack distribute step, so a file lands in the same folder however forage
// came to file it.
func topPerformerName(perfs []stash.ScenePerformer) string {
	var top stash.ScenePerformer
	for _, p := range perfs {
		if p.Name == "" {
			continue
		}
		if top.Name == "" || p.SceneCount > top.SceneCount {
			top = p
		}
	}
	return top.Name
}

// getUnfiled lists the library's unfiled scenes.
//
//	GET /unfiled?bucket=identified
func (s *Server) getUnfiled(w http.ResponseWriter, r *http.Request) {
	sc := s.pool.Stash()
	if sc == nil {
		writeErr(w, http.StatusServiceUnavailable, "Stash isn't configured")
		return
	}
	cfg := s.composedConfig()
	root := cfg.LibraryRoot
	if root == "" {
		writeErr(w, http.StatusUnprocessableEntity,
			"no library root configured, so there is no such thing as unfiled yet")
		return
	}
	// Ask Stash in ITS path vocabulary. forage's root is a container path; the
	// library it points at is the same directory Stash scanned under another
	// name, and a regex built from the wrong one silently matches nothing.
	stashRoot := pathmap.Translate(root, cfg.StashPathMapping)
	if stashRoot == "" {
		stashRoot = root
	}
	found, err := sc.FindUnfiledScenes(r.Context(), stashRoot)
	if err != nil {
		s.log.Warn("unfiled: query", "root", stashRoot, "err", err)
		writeErr(w, http.StatusBadGateway, "couldn't ask Stash: "+err.Error())
		return
	}

	want := r.URL.Query().Get("bucket")
	out := unfiledResponse{
		Scenes:      []unfiledScene{},
		Counts:      map[string]int{"filable": 0, "identified": 0, "unknown": 0},
		LibraryRoot: stashRoot,
	}
	for _, f := range found {
		b := bucketFor(f)
		out.Counts[b]++
		if want != "" && want != b {
			continue
		}
		row := unfiledScene{
			SceneID: f.ID, Title: f.Title, Path: f.FilePath,
			Bucket: b, Suggested: topPerformerName(f.Performers),
			Identified: len(f.StashIDs) > 0,
		}
		for _, p := range f.Performers {
			if p.Name != "" {
				row.Performers = append(row.Performers, p.Name)
			}
		}
		out.Scenes = append(out.Scenes, row)
	}
	sort.SliceStable(out.Scenes, func(i, j int) bool {
		return out.Scenes[i].Path < out.Scenes[j].Path
	})
	writeJSON(w, http.StatusOK, out)
}

type fileUnfiledRequest struct {
	SceneIDs []string `json:"scene_ids"`
	// Performer is the folder to file under. Required: this endpoint does not
	// guess. The list view already suggests one per scene, and a bulk action
	// whose destination is implicit is a bulk action nobody can predict.
	Performer string `json:"performer"`
}

// postUnfiledFile moves the named scenes' files under a performer folder.
//
//	POST /unfiled/file  {"scene_ids": [...], "performer": "Kenzie Reeves"}
//
// A MOVE, not a hardlink-and-unlink. These files are already in the library:
// the placer's copy-or-link dance exists to bring a file IN from a download
// client, and reusing it here would leave the original behind or delete it.
// os.Rename within one filesystem is atomic and cheap, and Stash re-attaches
// the scene by hash on the next scan rather than creating a duplicate.
//
// Never overwrites. The whole point of this endpoint is that nothing is lost.
func (s *Server) postUnfiledFile(w http.ResponseWriter, r *http.Request) {
	var req fileUnfiledRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	req.Performer = strings.TrimSpace(req.Performer)
	if req.Performer == "" || len(req.SceneIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "scene_ids and performer are both required")
		return
	}
	sc := s.pool.Stash()
	if sc == nil {
		writeErr(w, http.StatusServiceUnavailable, "Stash isn't configured")
		return
	}
	cfg := s.composedConfig()
	if cfg.LibraryRoot == "" {
		writeErr(w, http.StatusUnprocessableEntity, "no library root configured")
		return
	}
	stashRoot := pathmap.Translate(cfg.LibraryRoot, cfg.StashPathMapping)
	if stashRoot == "" {
		stashRoot = cfg.LibraryRoot
	}
	found, err := sc.FindUnfiledScenes(r.Context(), stashRoot)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "couldn't ask Stash: "+err.Error())
		return
	}
	// Only scenes the query returned may be moved. Filing by an id the caller
	// supplies without checking would let any scene in the library be moved
	// through an endpoint whose entire contract is "things that are unfiled".
	byID := map[string]stash.UnfiledScene{}
	for _, f := range found {
		byID[f.ID] = f
	}

	type outcome struct {
		SceneID string `json:"scene_id"`
		From    string `json:"from,omitempty"`
		To      string `json:"to,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	res := struct {
		Moved   int       `json:"moved"`
		Skipped int       `json:"skipped"`
		Results []outcome `json:"results"`
	}{Results: []outcome{}}

	dir := filepath.Join(cfg.LibraryRoot, sanitiseFolder(req.Performer))
	for _, id := range req.SceneIDs {
		f, ok := byID[id]
		if !ok {
			res.Skipped++
			res.Results = append(res.Results, outcome{SceneID: id,
				Error: "not in the unfiled set (already filed, or not a library scene)"})
			continue
		}
		src := pathmap.Reverse(f.FilePath, cfg.StashPathMapping)
		if src == "" {
			src = f.FilePath
		}
		if _, err := os.Stat(src); err != nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{SceneID: id, From: src,
				Error: "file is not where Stash says it is"})
			continue
		}
		dst := filepath.Join(dir, filepath.Base(src))
		if _, err := os.Stat(dst); err == nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{SceneID: id, From: src, To: dst,
				Error: "a file of that name is already there"})
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{SceneID: id, Error: err.Error()})
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{SceneID: id, From: src, To: dst,
				Error: err.Error()})
			continue
		}
		res.Moved++
		res.Results = append(res.Results, outcome{SceneID: id, From: src, To: dst})
		s.log.Info("unfiled: filed scene", "scene", id, "from", src, "to", dst,
			"performer", req.Performer)
	}

	// Stash still points at the old paths. Scan the destination only: a
	// library-wide scan is minutes on a large library, and the moved files are
	// all in one folder.
	if res.Moved > 0 {
		scanPath := pathmap.Translate(dir, cfg.StashPathMapping)
		if scanPath == "" {
			scanPath = dir
		}
		if job, err := sc.MetadataScan(r.Context(), []string{scanPath}); err != nil {
			s.log.Warn("unfiled: rescan", "path", scanPath, "err", err)
		} else {
			s.log.Info("unfiled: rescan queued", "path", scanPath, "job", job)
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// sanitiseFolder strips what a filesystem will not take in a directory name.
// Mirrors the placer's own cleaning; kept local so this endpoint does not
// depend on the placer's unexported helper.
func sanitiseFolder(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			return '_'
		}
		return r
	}, s)
	if s == "" {
		return "Unsorted"
	}
	return s
}
