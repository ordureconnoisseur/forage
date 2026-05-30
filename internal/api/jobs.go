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

// jobAutoPickFloor mirrors the plugin's AUTO_PICK_FLOOR: only auto-grab a
// scene whose best verified release clears this confidence. Below it the
// scene is skipped (left for manual review in the collection view).
const jobAutoPickFloor = 0.5

// jobSearchConcurrency bounds how many scenes a job searches at once —
// kept low for the same reason the plugin's collection search is (more
// concurrent Prowlarr fetches → trackers choke, "search failed").
const jobSearchConcurrency = 2

// jobRetention is how long a finished job stays listed before cleanup.
const jobRetention = 2 * time.Hour

type jobSceneStatus string

const (
	jobScenePending  jobSceneStatus = "pending"
	jobSceneGrabbed  jobSceneStatus = "grabbed"
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
}

type collectionJob struct {
	ID            string     `json:"id"`
	PerformerID   string     `json:"performer_id"`
	PerformerName string     `json:"performer_name"`
	State         string     `json:"state"` // running | done | cancelled
	Total         int        `json:"total"`
	Done          int        `json:"done"`    // scenes processed
	Grabbed       int        `json:"grabbed"` // scenes that produced a grab
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

// snapshot returns a deep-ish copy safe to marshal without holding the
// lock during JSON encoding.
func (j *collectionJob) snapshot() collectionJob {
	cp := *j
	cp.Scenes = append([]jobScene(nil), j.Scenes...)
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
	writeJSON(w, http.StatusOK, job.snapshot())
}

// getCollectionJobs lists jobs, newest first.
func (s *Server) getCollectionJobs(w http.ResponseWriter, r *http.Request) {
	s.jobs.mu.Lock()
	out := make([]collectionJob, 0, len(s.jobs.jobs))
	for _, j := range s.jobs.jobs {
		out = append(out, j.snapshot())
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

// processJobScene searches one scene, picks its best verified release
// over the auto-pick floor, and grabs it — recording the outcome.
func (s *Server) processJobScene(ctx context.Context, m *matcher.Matcher, job *collectionJob, i int, performerName string) {
	pc := s.pool.Prowlarr()
	stashDBC := s.pool.StashDB()
	if pc == nil || stashDBC == nil {
		s.setJobScene(job, i, jobSceneError, "")
		return
	}
	sceneID := job.Scenes[i].StashDBID
	scene, err := stashDBC.FindScene(ctx, sceneID)
	if err != nil || scene == nil {
		s.setJobScene(job, i, jobSceneError, "")
		return
	}

	perfNames := s.scenePerformerNames(ctx, scene, performerName, "")
	releases, err := s.searchSceneReleases(ctx, pc, scene, perfNames, s.pool.Settings().ProwlarrCategories, true /*lean*/)
	if err != nil {
		s.setJobScene(job, i, jobSceneError, "")
		return
	}
	if len(releases) == 0 {
		s.setJobScene(job, i, jobSceneNoResult, "")
		return
	}

	// Verify each release against this scene; keep the best one whose
	// confidence clears the auto-pick floor.
	var best *jobCandidate
	for _, rel := range releases {
		if ctx.Err() != nil {
			return
		}
		cands, mErr := m.Match(ctx, rel.Title)
		if mErr != nil {
			continue
		}
		vr := matcher.Verify(cands, sceneID, scene.Title, rel.Title)
		if !vr.Verified || vr.Confidence < jobAutoPickFloor {
			continue
		}
		if best == nil || vr.Confidence > best.conf {
			best = &jobCandidate{rel: rel.Title, url: rel.GrabURL(),
				size: rel.Size, indexer: rel.Indexer, protocol: rel.Protocol, conf: vr.Confidence}
		}
	}
	if best == nil {
		s.setJobScene(job, i, jobSceneNoMatch, "")
		return
	}

	_, gErr := s.doGrab(ctx, grabRequest{
		DownloadURL:    best.url,
		ReleaseTitle:   best.rel,
		ReleaseSize:    best.size,
		ReleaseIndexer: best.indexer,
		Protocol:       best.protocol,
		SceneID:        sceneID,
		Confidence:     best.conf,
		PerformerName:  performerName,
	})
	if gErr != nil {
		s.setJobScene(job, i, jobSceneError, best.rel)
		return
	}
	s.setJobScene(job, i, jobSceneGrabbed, best.rel)
}

type jobCandidate struct {
	rel, url, indexer, protocol string
	size                        int64
	conf                        float64
}

// setJobScene records a scene's terminal status + bumps the counters.
func (s *Server) setJobScene(job *collectionJob, i int, st jobSceneStatus, release string) {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	job.Scenes[i].Status = st
	job.Scenes[i].Release = release
	job.Done++
	if st == jobSceneGrabbed {
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
