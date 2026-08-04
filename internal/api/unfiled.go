package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ordureconnoisseur/forager/internal/pathmap"
	"github.com/ordureconnoisseur/forager/internal/placer"
	"github.com/ordureconnoisseur/forager/internal/stash"
	"github.com/ordureconnoisseur/forager/internal/suggest"
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

type unfiledResponse struct {
	Scenes []unfiledItem  `json:"scenes"`
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
	found, err := sc.FindUnfiledScenes(r.Context(), stashRoot, placer.UnfiledFolders())
	if err != nil {
		s.log.Warn("unfiled: query", "root", stashRoot, "err", err)
		writeErr(w, http.StatusBadGateway, "couldn't ask Stash: "+err.Error())
		return
	}

	want := r.URL.Query().Get("bucket")
	// ConfidentTopFolder, not the ranked picker. This suggestion pre-fills
	// the destination for a selection, and one row here can be 489 files, so
	// a confidently wrong name is far worse than none.
	//
	// Measured on the reference library with the ranked picker: "Adorable
	// Alice Video Pack" suggested Ember Snow and "Ella Hollywood
	// [Onlyfans.com]" suggested hungdoll. The conservative variant exists for
	// exactly this. It answers only when the best match is a full multi-word
	// name with no rival matching as many words, which is the same bar
	// adoption uses before it auto-files a torrent, and for the same reason:
	// a lone first-name hit is the false-positive class that mis-filed a
	// batch of them.
	//
	// The ranked picker still drives the one-click chips, where a weaker
	// guess is an option someone chooses rather than a default they inherit.
	items, counts := groupUnfiled(found, stashRoot, want, func(name string) string {
		return suggest.ConfidentTopFolder(r.Context(), s.db, name)
	})
	out := unfiledResponse{Scenes: items, Counts: counts, LibraryRoot: stashRoot}
	if out.Scenes == nil {
		out.Scenes = []unfiledItem{}
	}
	writeJSON(w, http.StatusOK, out)
}

type fileUnfiledRequest struct {
	// Keys are what the list handed out: a Stash scene id for a loose file,
	// a folder path for a pack. Opaque to the caller on purpose, so the two
	// cannot be confused for one another.
	Keys []string `json:"keys"`
	// SceneIDs is the older name, still accepted so an in-flight page does
	// not fail after a deploy.
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
	keys := req.Keys
	if len(keys) == 0 {
		keys = req.SceneIDs
	}
	if req.Performer == "" || len(keys) == 0 {
		writeErr(w, http.StatusBadRequest, "keys and performer are both required")
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
	found, err := sc.FindUnfiledScenes(r.Context(), stashRoot, placer.UnfiledFolders())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "couldn't ask Stash: "+err.Error())
		return
	}
	// Only what the LIST offered may be moved. Filing an arbitrary path or
	// scene id supplied by the caller would let anything in the library be
	// relocated through an endpoint whose whole contract is "things that are
	// unfiled", and for a pack that path is a directory.
	items, _ := groupUnfiled(found, stashRoot, "", func(string) string { return "" })
	offered := map[string]unfiledItem{}
	for _, it := range items {
		offered[it.Key] = it
	}

	type outcome struct {
		Key   string `json:"key"`
		From  string `json:"from,omitempty"`
		To    string `json:"to,omitempty"`
		Files int    `json:"files,omitempty"`
		Error string `json:"error,omitempty"`
	}
	res := struct {
		Moved   int       `json:"moved"`
		Skipped int       `json:"skipped"`
		Results []outcome `json:"results"`
	}{Results: []outcome{}}

	dir := filepath.Join(cfg.LibraryRoot, sanitiseFolder(req.Performer))
	// One snapshot for the whole batch. Filing MOVES files, and a move is what
	// breaks a torrent: the client keeps pointing at the old path and the
	// download goes dark without any error anywhere. Ten torrents on this
	// library died exactly that way, to a bulk move of loose files that never
	// asked what was being served.
	seeded := s.seedingSet(r.Context())
	for _, key := range keys {
		it, ok := offered[key]
		if !ok {
			res.Skipped++
			res.Results = append(res.Results, outcome{Key: key,
				Error: "not in the unfiled set (already filed, or not a library scene)"})
			continue
		}
		src := pathmap.Reverse(it.Path, cfg.StashPathMapping)
		if src == "" {
			src = it.Path
		}
		if _, err := os.Stat(src); err != nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{Key: key, From: src,
				Error: "not where Stash says it is"})
			continue
		}
		// A seeded path is refused however filable it looks. Unlike the other
		// refusals here this one is about a system OUTSIDE the library: the
		// file is fine, the library would be fine, and the torrent would
		// silently stop. Blocks (not Covers) because a pack folder moves whole
		// and the torrent is usually seeding a file INSIDE it.
		if cp := seeded.Blocks(src); cp != "" {
			res.Skipped++
			res.Results = append(res.Results, outcome{Key: key, From: src,
				Error: "a torrent is seeding " + cp + " — moving this would break it"})
			continue
		}
		// A pack moves whole, under <performer>/<pack name>/. Moving its
		// files individually would gut a folder the poller's own distribute
		// step goes out of its way to preserve, and would scatter a release
		// across a performer folder it was never part of.
		dst := filepath.Join(dir, filepath.Base(src))
		if _, err := os.Stat(dst); err == nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{Key: key, From: src, To: dst,
				Error: "something of that name is already there"})
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{Key: key, Error: err.Error()})
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			res.Skipped++
			res.Results = append(res.Results, outcome{Key: key, From: src, To: dst,
				Error: err.Error()})
			continue
		}
		res.Moved++
		res.Results = append(res.Results, outcome{Key: key, From: src, To: dst, Files: it.Files})
		s.log.Info("unfiled: filed", "kind", it.Kind, "key", key, "from", src,
			"to", dst, "files", it.Files, "performer", req.Performer)
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

// postUnfiledSuggest ranks local performers for a selection of unfiled files.
//
//	POST /unfiled/suggest  {"scene_ids": [...]}
//
// Same ranking the grab detail offers for a mis-filed grab (suggest.Performers
// over the release name), so the two screens agree about who a file is likely
// to belong to. Reusing it matters: the naive version of this is a plain text
// box, which asks the user to remember and retype a name the daemon can
// already guess from the filename.
//
// Scoped to the SELECTION rather than returned per row. The list endpoint
// already ships 4,887 rows on the reference library, and attaching six ranked
// performers to each would multiply a payload that is mostly scrolled past.
func (s *Server) postUnfiledSuggest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys     []string `json:"keys"`
		SceneIDs []string `json:"scene_ids"` // older name, still accepted
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	keys := req.Keys
	if len(keys) == 0 {
		keys = req.SceneIDs
	}
	if len(keys) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"suggestions": []suggestedPerformer{}})
		return
	}
	sc := s.pool.Stash()
	if sc == nil {
		writeErr(w, http.StatusServiceUnavailable, "Stash isn't configured")
		return
	}
	cfg := s.composedConfig()
	stashRoot := pathmap.Translate(cfg.LibraryRoot, cfg.StashPathMapping)
	if stashRoot == "" {
		stashRoot = cfg.LibraryRoot
	}
	found, err := sc.FindUnfiledScenes(r.Context(), stashRoot, placer.UnfiledFolders())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "couldn't ask Stash: "+err.Error())
		return
	}
	items, _ := groupUnfiled(found, stashRoot, "", func(string) string { return "" })
	byKey := map[string]unfiledItem{}
	for _, it := range items {
		byKey[it.Key] = it
	}

	// Rank across the whole selection, best total first. A performer named by
	// several of the selected items beats one named by the first, which is
	// what matters after filtering to a name and selecting everything.
	//
	// For a pack the string to read is the FOLDER's name, not a member file's.
	// Stash names one performer across a whole pack in 6 of 103 folders on the
	// reference library and nobody at all in 93, while the folder itself is
	// called "Toni Camille pack".
	score := map[string]int{}
	meta := map[string]suggestedPerformer{}
	for _, key := range keys {
		it, ok := byKey[key]
		if !ok {
			continue
		}
		subject := it.Name
		for _, p := range s.suggestPerformers(r.Context(), subject) {
			score[p.Name] += p.SceneCount + 1
			meta[p.Name] = p
		}
		// A pack whose folder name says nothing still has whatever Stash
		// managed to name inside it.
		if len(meta) == 0 && it.Suggested != "" {
			meta[it.Suggested] = suggestedPerformer{Name: it.Suggested}
			score[it.Suggested]++
		}
	}
	out := make([]suggestedPerformer, 0, len(meta))
	for name := range meta {
		out = append(out, meta[name])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if score[out[i].Name] != score[out[j].Name] {
			return score[out[i].Name] > score[out[j].Name]
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 6 {
		out = out[:6]
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
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
		// A name that sanitises away entirely is no name at all.
		return placer.UnfiledFolder
	}
	return s
}
