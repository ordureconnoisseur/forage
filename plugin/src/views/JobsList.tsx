import { useEffect, useRef, useState } from "react";
import {
  cancelCollectionJob,
  fetchCollectionJobs,
  type CollectionJob,
  type JobScene,
} from "../api";

// Poll cadence: brisk while a job runs, slow when everything's settled.
const FAST_MS = 3000;
const SLOW_MS = 15000;

export default function JobsList({
  onPickPerformer,
  onReview,
}: {
  onPickPerformer: (id: string) => void;
  // Re-open a job as the interactive collection view (pick releases for
  // scenes the auto-pass skipped).
  onReview: (jobId: string, performerId: string) => void;
}) {
  const [jobs, setJobs] = useState<CollectionJob[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Bumped on cancel to refetch immediately (don't wait out the 15s poll).
  const [reloadNonce, setReloadNonce] = useState(0);
  const timer = useRef<number | undefined>(undefined);

  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      try {
        const r = await fetchCollectionJobs();
        if (cancelled) return;
        setJobs(r.jobs);
        setError(null);
        const anyRunning = r.jobs.some((j) => j.state === "running");
        timer.current = window.setTimeout(tick, anyRunning ? FAST_MS : SLOW_MS);
      } catch (e) {
        if (cancelled) return;
        setError((e as Error).message);
        timer.current = window.setTimeout(tick, SLOW_MS);
      }
    };
    void tick();
    return () => {
      cancelled = true;
      if (timer.current) clearTimeout(timer.current);
    };
  }, [reloadNonce]);

  if (error && !jobs)
    return <div className="empty error">Failed to load jobs: {error}</div>;
  if (!jobs) return <div className="empty">Loading jobs…</div>;
  if (jobs.length === 0)
    return (
      <div className="empty">
        No collection jobs. Start one from a performer's “Complete collection”
        with “Search on server” — it searches every scene in the background
        (you can close this tab), then you Review and pick what to grab.
        Nothing is grabbed automatically.
      </div>
    );

  return (
    <div>
      <div className="page-header">
        <h2>Jobs</h2>
        <div className="meta">
          Server-side scene searches — they keep running if you leave. Open one
          to Review and grab what you want; nothing grabs on its own.
        </div>
      </div>
      <ul className="job-list">
        {jobs.map((j) => (
          <JobCard
            key={j.id}
            job={j}
            onPickPerformer={onPickPerformer}
            onReview={onReview}
            onChanged={() => setReloadNonce((n) => n + 1)}
          />
        ))}
      </ul>
    </div>
  );
}

function JobCard({
  job,
  onPickPerformer,
  onReview,
  onChanged,
}: {
  job: CollectionJob;
  onPickPerformer: (id: string) => void;
  onReview: (jobId: string, performerId: string) => void;
  // Triggers an immediate list refetch (after a cancel) instead of waiting
  // for the next poll.
  onChanged: () => void;
}) {
  const [cancelling, setCancelling] = useState(false);
  const pct = job.total > 0 ? (job.done / job.total) * 100 : 0;

  const cancel = async () => {
    setCancelling(true);
    try {
      await cancelCollectionJob(job.id);
      onChanged(); // refetch now — the job is gone from the server
    } catch {
      setCancelling(false);
    }
  };

  // Tally outcomes for the summary line.
  const counts = job.scenes.reduce<Record<string, number>>((acc, s) => {
    acc[s.status] = (acc[s.status] || 0) + 1;
    return acc;
  }, {});

  return (
    <li className={"job-card state-" + job.state}>
      <div className="job-head">
        <button
          className="job-performer"
          onClick={() => onPickPerformer(job.performer_id)}
        >
          {job.performer_name}
        </button>
        <span className={"job-state " + job.state}>
          {job.state === "running" && <span className="coll-spinner" />}
          {job.state}
        </span>
        {job.state === "running" ? (
          <button
            className="job-cancel"
            onClick={cancel}
            disabled={cancelling}
          >
            {cancelling ? "cancelling…" : "Cancel"}
          </button>
        ) : (
          <button
            className="job-review"
            onClick={() => onReview(job.id, job.performer_id)}
            title="Open the found releases — review, adjust picks, and grab what you want"
          >
            Review &amp; grab →
          </button>
        )}
      </div>

      <div className="job-progress-track">
        <div
          className="job-progress-fill"
          style={{ width: `${pct}%` }}
        />
      </div>

      <div className="job-summary">
        <span>
          {job.done}/{job.total} searched
        </span>
        <span className="sep">·</span>
        <span className="job-grabbed">{job.found} ready to grab</span>
        {job.grabbed ? (
          <>
            <span className="sep">·</span>
            <span>{job.grabbed} grabbed</span>
          </>
        ) : null}
        {counts.skipped ? (
          <>
            <span className="sep">·</span>
            <span>{counts.skipped} already grabbing</span>
          </>
        ) : null}
        {counts.no_match || counts.no_result ? (
          <>
            <span className="sep">·</span>
            <span>
              {(counts.no_match || 0) + (counts.no_result || 0)} no match
            </span>
          </>
        ) : null}
        {counts.error ? (
          <>
            <span className="sep">·</span>
            <span className="job-err">{counts.error} error</span>
          </>
        ) : null}
      </div>

      <JobScenes scenes={job.scenes} />
    </li>
  );
}

// JobScenes is a collapsible per-scene breakdown — collapsed by default
// (the summary line is usually enough), expandable to see each outcome.
function JobScenes({ scenes }: { scenes: JobScene[] }) {
  return (
    <details className="job-scenes">
      <summary>{scenes.length} scenes</summary>
      <ul className="job-scene-list">
        {scenes.map((s) => (
          <li key={s.stashdb_id} className={"job-scene st-" + s.status}>
            <span className="job-scene-status">{s.status}</span>
            <span className="job-scene-title">{s.title || "(untitled)"}</span>
            {s.release && (
              <code className="job-scene-release">{s.release}</code>
            )}
          </li>
        ))}
      </ul>
    </details>
  );
}
