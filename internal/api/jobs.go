package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ordureconnoisseur/forager/internal/matcher"
)

// Collection jobs run a "complete the collection" / multi-scene grab on
// the DAEMON instead of the browser, so the crawl survives the user
// reloading or closing the plugin. In-memory (v1): a job is lost on a
// daemon restart, but every grab it already queued persists in the grabs
// table regardless, so the worst case is the remaining queue stops — no
// lost downloads. The browser polls /jobs for progress.

// jobSuggestFloor mirrors the plugin's AUTO_PICK_FLOOR: only auto-grab a
// scene whose best verified release clears this confidence. Below it the
// scene is skipped (left for manual review in the collection view).
const jobSuggestFloor = 0.5

// jobSearchConcurrency bounds how many scenes a job searches at once —
// kept low for the same reason the plugin's collection search is (more
// concurrent Prowlarr fetches → trackers choke, "search failed").
const jobSearchConcurrency = 2

// jobRetention is how long a finished job stays listed before cleanup.
const jobRetention = 2 * time.Hour

type jobSceneStatus string

const (
	jobScenePending  jobSceneStatus = "pending"
	jobSceneFound    jobSceneStatus = "found"     // searched, has a suggested pick — awaiting your grab
	jobSceneGrabbed  jobSceneStatus = "grabbed"   // you grabbed it (from Review)
	jobSceneNoMatch  jobSceneStatus = "no_match"  // searched, nothing confident
	jobSceneNoResult jobSceneStatus = "no_result" // searched, zero releases
	jobSceneError    jobSceneStatus = "error"
	jobSceneSkipped  jobSceneStatus = "skipped" // already owned / in flight
)

type jobScene struct {
	StashDBID string         `json:"stashdb_id"`
	Title     string         `json:"title"`
	Status    jobSceneStatus `json:"status"`
	Release   string         `json:"release,omitempty"` // chosen release title when grabbed
	// Candidates is the full verified release list for this scene — stored
	// so a finished job re-opens as the identical interactive collection
	// view (expand candidates, re-pick the ones auto-pick skipped). Empty
	// until the scene is searched; omitted from the list endpoint to keep
	// it light (see jobDetail).
	Candidates []sceneRelease `json:"candidates,omitempty"`
	// PickedURL is the grab URL of the chosen release (auto-pick's choice,
	// or the user's later re-pick), "" when nothing is selected.
	PickedURL string `json:"picked_url,omitempty"`
}

type collectionJob struct {
	ID            string     `json:"id"`
	PerformerID   string     `json:"performer_id"`
	PerformerName string     `json:"performer_name"`
	State         string     `json:"state"` // running | done | cancelled
	Total         int        `json:"total"`
	Done          int        `json:"done"`    // scenes processed
	Found         int        `json:"found"`   // scenes with a suggested pick (ready to grab)
	Grabbed       int        `json:"grabbed"` // scenes you actually grabbed (from Review)
	StartedAt     int64      `json:"started_at"`
	FinishedAt    int64      `json:"finished_at,omitempty"`
	Scenes        []jobScene `json:"scenes"`
	cancel        context.CancelFunc
}

// jobStore is the in-memory registry of collection jobs.
type jobStore struct {
	mu   sync.Mutex
	jobs map[string]*collectionJob
	seq  int
}

func newJobStore() *jobStore { return &jobStore{jobs: map[string]*collectionJob{}} }

// snapshot returns a copy safe to marshal without holding the lock during
// JSON encoding. When light, per-scene Candidates are dropped (the list
// endpoint doesn't need the full release sets — only the detail does).
func (j *collectionJob) snapshot(light bool) collectionJob {
	cp := *j
	cp.Scenes = make([]jobScene, len(j.Scenes))
	for i, sc := range j.Scenes {
		if light {
			sc.Candidates = nil
		} else {
			sc.Candidates = append([]sceneRelease(nil), sc.Candidates...)
		}
		cp.Scenes[i] = sc
	}
	cp.cancel = nil
	return cp
}

// ── HTTP handlers ────────────────────────────────────────────────────

type startJobRequest struct {
	PerformerID string `json:"performer_id"`
	// Optional subset of StashDB scene ids (the multi-select). Empty =
	// every missing scene for the performer.
	SceneIDs []string `json:"scene_ids,omitempty"`
}

// postCollectionJob starts a server-side collection crawl and returns the
// job id immediately. The crawl continues regardless of the client.
func (s *Server) postCollectionJob(w http.ResponseWriter, r *http.Request) {
	var req startJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.PerformerID == "" {
		writeErr(w, http.StatusBadRequest, "performer_id required")
		return
	}
	job, err := s.startCollectionJob(req.PerformerID, req.SceneIDs)
	if err != nil {
		var ge grabError
		status := http.StatusBadGateway
		msg := err.Error()
		if errors.As(err, &ge) {
			status, msg = ge.status, ge.msg
		}
		writeErr(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot(true))
}

// getCollectionJobs lists jobs, newest first (light — no candidate lists).
func (s *Server) getCollectionJobs(w http.ResponseWriter, r *http.Request) {
	s.jobs.mu.Lock()
	out := make([]collectionJob, 0, len(s.jobs.jobs))
	for _, j := range s.jobs.jobs {
		out = append(out, j.snapshot(true))
	}
	s.jobs.mu.Unlock()
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt > out[k].StartedAt })
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// deleteCollectionJob cancels a running job (and drops it from the list).
func (s *Server) deleteCollectionJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.jobs.mu.Lock()
	j := s.jobs.jobs[id]
	if j != nil {
		if j.cancel != nil {
			j.cancel()
		}
		if j.State == "running" {
			j.State = "cancelled"
			j.FinishedAt = time.Now().Unix()
		}
	}
	s.jobs.mu.Unlock()
	if j == nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// getCollectionJobDetail returns one job WITH per-scene candidate lists,
// so the plugin can re-open it as the full interactive collection view.
func (s *Server) getCollectionJobDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	s.jobs.mu.Lock()
	j := s.jobs.jobs[id]
	var snap collectionJob
	if j != nil {
		snap = j.snapshot(false)
	}
	s.jobs.mu.Unlock()
	if j == nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

type jobGrabRequest struct {
	SceneID     string `json:"scene_id"`
	DownloadURL string `json:"download_url"` // which candidate to grab
}

// postCollectionJobGrab grabs a specific candidate for one scene of a job
// — the re-pick path: the user opened a finished job and chose a release
// for a scene the auto-pass skipped (or wants to override). Looks the
// candidate up in the stored list, grabs it, and flips the scene to
// grabbed.
func (s *Server) postCollectionJobGrab(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req jobGrabRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	s.jobs.mu.Lock()
	j := s.jobs.jobs[id]
	var (
		perfName string
		sceneIdx = -1
		cand     *sceneRelease
	)
	if j != nil {
		perfName = j.PerformerName
		for i := range j.Scenes {
			if j.Scenes[i].StashDBID == req.SceneID {
				sceneIdx = i
				for k := range j.Scenes[i].Candidates {
					if j.Scenes[i].Candidates[k].DownloadURL == req.DownloadURL {
						c := j.Scenes[i].Candidates[k]
						cand = &c
					}
				}
				break
			}
		}
	}
	s.jobs.mu.Unlock()
	if j == nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if sceneIdx < 0 || cand == nil {
		writeErr(w, http.StatusNotFound, "scene/candidate not found in job")
		return
	}

	_, err := s.doGrab(r.Context(), grabRequest{
		DownloadURL:    cand.DownloadURL,
		ReleaseTitle:   cand.Title,
		ReleaseSize:    cand.Size,
		ReleaseIndexer: cand.Indexer,
		Protocol:       cand.Protocol,
		SceneID:        req.SceneID,
		Confidence:     cand.Confidence,
		PerformerName:  perfName,
	})
	if err != nil {
		var ge grabError
		if errors.As(err, &ge) {
			writeErr(w, ge.status, ge.msg)
			return
		}
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	// Flip the scene to grabbed in place.
	s.jobs.mu.Lock()
	if sceneIdx < len(j.Scenes) {
		sc := &j.Scenes[sceneIdx]
		wasGrabbed := sc.Status == jobSceneGrabbed
		sc.Status = jobSceneGrabbed
		sc.Release = cand.Title
		sc.PickedURL = cand.DownloadURL
		if !wasGrabbed {
			j.Grabbed++
		}
	}
	s.jobs.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Worker ───────────────────────────────────────────────────────────

// startCollectionJob resolves the target scene set and launches the
// background crawl. Returns the job with its scene list populated.
func (s *Server) startCollectionJob(performerID string, sceneIDs []string) (*collectionJob, error) {
	ctx := context.Background()
	stashDBC := s.pool.StashDB()
	if stashDBC == nil || s.pool.Prowlarr() == nil {
		return nil, grabError{http.StatusServiceUnavailable, "prowlarr and stashdb must be configured"}
	}
	perf, err := loadPerformerByID(ctx, s.db, performerID)
	if err != nil || perf == nil {
		return nil, grabError{http.StatusNotFound, "performer not found"}
	}
	sdbID, err := lookupStashDBPerformerID(ctx, s.db, performerID)
	if err != nil || sdbID == "" {
		return nil, grabError{http.StatusUnprocessableEntity, "performer has no StashDB cross-id"}
	}

	scenes, err := s.performerFilmography(ctx, stashDBC, sdbID)
	if err != nil {
		return nil, grabError{http.StatusBadGateway, "stashdb: " + err.Error()}
	}
	owned, err := s.ownedStashDBSet(ctx)
	if err != nil {
		return nil, grabError{http.StatusBadGateway, "stash: " + err.Error()}
	}
	inFlight, _ := s.grabs.StatusByStashDBID(ctx)

	want := map[string]bool{}
	for _, id := range sceneIDs {
		want[id] = true
	}

	var js []jobScene
	for _, sc := range scenes {
		if owned[sc.ID] {
			continue // already in library
		}
		if len(want) > 0 && !want[sc.ID] {
			continue // not in the selected subset
		}
		st := jobScenePending
		if inFlight[sc.ID] != "" {
			st = jobSceneSkipped // already grabbed / downloading
		}
		js = append(js, jobScene{StashDBID: sc.ID, Title: sc.Title, Status: st})
	}
	if len(js) == 0 {
		return nil, grabError{http.StatusUnprocessableEntity, "no missing scenes to grab for this performer"}
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	s.jobs.mu.Lock()
	s.jobs.seq++
	id := time.Now().Format("20060102-150405") + "-" + strconv.Itoa(s.jobs.seq)
	job := &collectionJob{
		ID:            id,
		PerformerID:   performerID,
		PerformerName: perf.Name,
		State:         "running",
		Total:         len(js),
		StartedAt:     time.Now().Unix(),
		Scenes:        js,
		cancel:        cancel,
	}
	s.jobs.jobs[id] = job
	s.jobs.mu.Unlock()

	go s.runCollectionJob(jobCtx, job, perf.Name)
	s.log.Info("collection job started", "id", id, "performer", perf.Name, "scenes", len(js))
	return job, nil
}

// runCollectionJob crawls the job's scene list, grabbing the best
// verified release for each. Bounded concurrency; updates job state in
// place under the store lock so /jobs reflects live progress.
func (s *Server) runCollectionJob(ctx context.Context, job *collectionJob, performerName string) {
	m, err := s.Matcher(ctx)
	if err != nil {
		s.finishJob(job, "done")
		s.log.Error("collection job: matcher unavailable", "id", job.ID, "err", err)
		return
	}

	queue := make(chan int)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for i := range queue {
			if ctx.Err() != nil {
				return
			}
			s.processJobScene(ctx, m, job, i, performerName)
		}
	}
	for w := 0; w < jobSearchConcurrency; w++ {
		wg.Add(1)
		go worker()
	}
	for i := range job.Scenes {
		s.jobs.mu.Lock()
		skip := job.Scenes[i].Status == jobSceneSkipped
		if skip {
			job.Done++
		}
		s.jobs.mu.Unlock()
		if skip {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		queue <- i
	}
	close(queue)
	wg.Wait()

	state := "done"
	if ctx.Err() != nil {
		state = "cancelled"
	}
	s.finishJob(job, state)
	s.log.Info("collection job finished", "id", job.ID, "state", state,
		"grabbed", job.Grabbed, "total", job.Total)
	go s.cleanupJobLater(job.ID)
}

// processJobScene SEARCHES one scene and stores its full verified
// candidate list, with the best confident release PRE-SELECTED (picked)
// as a suggestion. It never grabs — grabbing happens only when the user
// opens Review and hits "Grab selected". This makes a job a background
// search that survives leaving the page; the human always decides.
func (s *Server) processJobScene(ctx context.Context, m *matcher.Matcher, job *collectionJob, i int, performerName string) {
	pc := s.pool.Prowlarr()
	stashDBC := s.pool.StashDB()
	if pc == nil || stashDBC == nil {
		s.setJobScene(job, i, jobSceneError, "", nil, "")
		return
	}
	sceneID := job.Scenes[i].StashDBID
	scene, err := stashDBC.FindScene(ctx, sceneID)
	if err != nil || scene == nil {
		s.setJobScene(job, i, jobSceneError, "", nil, "")
		return
	}

	perfNames := s.scenePerformerNames(ctx, scene, performerName, "")
	releases, err := s.searchSceneReleases(ctx, pc, scene, perfNames, s.pool.Settings().ProwlarrCategories, true /*lean*/)
	if err != nil {
		s.setJobScene(job, i, jobSceneError, "", nil, "")
		return
	}
	if len(releases) == 0 {
		s.setJobScene(job, i, jobSceneNoResult, "", nil, "")
		return
	}

	// Shape + verify all candidates (same logic as the interactive view),
	// ranked confidence-desc so the strongest leads.
	cands := s.verifyReleases(ctx, m, sceneID, scene.Title, releases)
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].Confidence > cands[b].Confidence })

	// Pre-select the best confident release as a SUGGESTION (not a grab).
	pickURL, pickTitle := "", ""
	for k := range cands {
		if cands[k].Verified && cands[k].Confidence >= jobSuggestFloor {
			pickURL, pickTitle = cands[k].DownloadURL, cands[k].Title
			break
		}
	}
	if pickURL == "" {
		s.setJobScene(job, i, jobSceneNoMatch, "", cands, "")
		return
	}
	s.setJobScene(job, i, jobSceneFound, pickTitle, cands, pickURL)
}

// setJobScene records a scene's terminal status + candidates + bumps the
// counters.
func (s *Server) setJobScene(job *collectionJob, i int, st jobSceneStatus, release string, cands []sceneRelease, pickedURL string) {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	job.Scenes[i].Status = st
	job.Scenes[i].Release = release
	job.Scenes[i].Candidates = cands
	job.Scenes[i].PickedURL = pickedURL
	job.Done++
	switch st {
	case jobSceneFound:
		job.Found++
	case jobSceneGrabbed:
		job.Grabbed++
	}
}

func (s *Server) finishJob(job *collectionJob, state string) {
	s.jobs.mu.Lock()
	if job.State == "running" {
		job.State = state
		job.FinishedAt = time.Now().Unix()
	}
	s.jobs.mu.Unlock()
}

// cleanupJobLater drops a finished job from the store after jobRetention.
func (s *Server) cleanupJobLater(id string) {
	timer := time.NewTimer(jobRetention)
	<-timer.C
	s.jobs.mu.Lock()
	delete(s.jobs.jobs, id)
	s.jobs.mu.Unlock()
}
