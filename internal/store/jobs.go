package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// JobStatus is the lifecycle of an agent job.
type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
)

// Job is one agent run for a task on a repo.
type Job struct {
	ID          int64
	JiraKey     string
	RepoName    string
	Status      JobStatus
	Branch      string
	MRURL       string
	Error       string
	TestsPassed *bool // nil = tests not run
	CreatedAt   time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	MRIID       int64
	MergedSHA   string
	MergedAt    time.Time // non-zero = merged into base_branch, waiting for handover
	HandoverID  int64
	DeliveredAt time.Time // non-zero = handover CI/CD succeeded
	WIPSHA      string    // failed job: partial agent work committed on Branch (local only, never pushed)
	ResumeOf    int64     // continues the WIP of this job (0 = fresh start)
	Attempt     int       // automatic retries so far in this chain (manual Retry resets it)
	NotBefore   time.Time // queued: not claimed before this (auto-retry backoff)
	AgentPGID   int       // running: process group of the agent (reaped after a worker crash)
	BootID      string    // host boot AgentPGID belongs to (a PGID from another boot is meaningless)
}

// jobColumns is shared by every SELECT/RETURNING so scanJob always matches.
const jobColumns = `id, jira_key, repo_name, status, branch, mr_url, error, tests_passed,
	created_at, started_at, finished_at, mr_iid, merged_sha, merged_at, handover_id, delivered_at,
	wip_sha, resume_of, attempt, not_before, agent_pgid, boot_id`

const selectJob = `SELECT ` + jobColumns + ` FROM jobs`

func scanJob(r scanner) (Job, error) {
	var (
		j                 Job
		passed            sql.NullInt64
		created           int64
		started, finished sql.NullInt64
		merged, handover  sql.NullInt64
		delivered         sql.NullInt64
		resumeOf, notBef  sql.NullInt64
	)
	if err := r.Scan(&j.ID, &j.JiraKey, &j.RepoName, &j.Status, &j.Branch, &j.MRURL, &j.Error, &passed,
		&created, &started, &finished, &j.MRIID, &j.MergedSHA, &merged, &handover, &delivered,
		&j.WIPSHA, &resumeOf, &j.Attempt, &notBef, &j.AgentPGID, &j.BootID); err != nil {
		return Job{}, err
	}
	j.MergedAt = fromNull(merged)
	j.HandoverID = handover.Int64
	j.DeliveredAt = fromNull(delivered)
	j.ResumeOf = resumeOf.Int64
	j.NotBefore = fromNull(notBef)
	if passed.Valid {
		p := passed.Int64 == 1
		j.TestsPassed = &p
	}
	j.CreatedAt = time.Unix(created, 0)
	j.StartedAt = fromNull(started)
	j.FinishedAt = fromNull(finished)
	return j, nil
}

// ApproveAndEnqueue approves the task and queues an agent job on repo, atomically.
func (s *Store) ApproveAndEnqueue(ctx context.Context, key, repo string, now time.Time) (Task, Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, Job{}, err
	}
	defer tx.Rollback()

	t, err := transitionTx(ctx, tx, key, []State{StateNew, StateNotified, StateSnoozed}, StateApproved,
		"remind_at = NULL", nil, "approved", repo, now)
	if err != nil {
		return Task{}, Job{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO jobs (jira_key, repo_name, status, created_at) VALUES (?, ?, ?, ?)`,
		key, repo, JobQueued, now.Unix())
	if err != nil {
		return Task{}, Job{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Task{}, Job{}, err
	}
	j, err := scanJob(tx.QueryRowContext(ctx, selectJob+` WHERE id = ?`, id))
	if err != nil {
		return Task{}, Job{}, err
	}
	return t, j, tx.Commit()
}

// LatestJob returns the most recent job for a task, or sql.ErrNoRows.
func (s *Store) LatestJob(ctx context.Context, key string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, selectJob+` WHERE jira_key = ? ORDER BY id DESC LIMIT 1`, key))
}

// GetJob returns a job by id.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, selectJob+` WHERE id = ?`, id))
}

// ClaimJob atomically moves the oldest due queued job to running (jobs waiting for a retry
// backoff are skipped until not_before). ok=false when nothing is due.
func (s *Store) ClaimJob(ctx context.Context, now time.Time) (Job, bool, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `UPDATE jobs SET status = ?, started_at = ?
		WHERE id = (SELECT id FROM jobs WHERE status = ? AND (not_before IS NULL OR not_before <= ?) ORDER BY id LIMIT 1)
		RETURNING `+jobColumns, JobRunning, now.Unix(), JobQueued, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// FailOrphaned marks jobs left running (e.g. worker crashed) as failed. Call at worker start.
func (s *Store) FailOrphaned(ctx context.Context, now time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE jobs SET status = ?, error = 'interrupted (worker restarted)', finished_at = ?
		WHERE status = ? RETURNING `+jobColumns, JobFailed, now.Unix(), JobRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetJobAgent records the running agent's process group so a restarted worker can reap it.
func (s *Store) SetJobAgent(ctx context.Context, id int64, pgid int, bootID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET agent_pgid = ?, boot_id = ? WHERE id = ?`, pgid, bootID, id)
	return err
}

// SetJobWIP records partial work salvaged from a failed job's worktree.
func (s *Store) SetJobWIP(ctx context.Context, id int64, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET wip_sha = ? WHERE id = ?`, sha, id)
	return err
}

// SetJobBranch records the branch a running job works on.
func (s *Store) SetJobBranch(ctx context.Context, id int64, branch string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET branch = ? WHERE id = ?`, branch, id)
	return err
}

// JobResult is the outcome of a finished job.
type JobResult struct {
	Status      JobStatus // JobSucceeded or JobFailed
	MRURL       string
	Error       string
	TestsPassed *bool
	MRIID       int64
	MergedSHA   string // set = the MR was merged into base_branch (job waits for handover)
	WIPSHA      string // failed: partial agent work committed on the job's local branch
}

// FinishJob stores a job's outcome and logs an event on its task.
func (s *Store) FinishJob(ctx context.Context, id int64, r JobResult, now time.Time) (Job, error) {
	var passed sql.NullInt64
	if r.TestsPassed != nil {
		passed.Valid = true
		if *r.TestsPassed {
			passed.Int64 = 1
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	var merged sql.NullInt64
	if r.MergedSHA != "" {
		merged = sql.NullInt64{Int64: now.Unix(), Valid: true}
	}
	j, err := scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET status = ?, mr_url = ?, error = ?, tests_passed = ?, finished_at = ?,
		mr_iid = ?, merged_sha = ?, merged_at = ?, wip_sha = ?, agent_pgid = 0
		WHERE id = ? RETURNING `+jobColumns, r.Status, r.MRURL, r.Error, passed, now.Unix(),
		r.MRIID, r.MergedSHA, merged, r.WIPSHA, id))
	if err != nil {
		return Job{}, err
	}
	payload := r.MRURL
	if r.MergedSHA != "" {
		payload = "merged " + r.MergedSHA + " " + r.MRURL
	}
	if r.Status == JobFailed {
		payload = r.Error
	}
	if err := addEvent(ctx, tx, j.JiraKey, "job_"+string(r.Status), payload, now); err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}

// ErrJobActive means the task already has a queued or running job.
var ErrJobActive = errors.New("a job is already queued or running for this task")

// Enqueue queues another job (manual retry / other repo) for an already approved task.
// A retry on the same repo continues the partial work (WIP) of the latest failed job.
func (s *Store) Enqueue(ctx context.Context, key, repo string, now time.Time) (Job, error) {
	return s.enqueue(ctx, key, repo, 0, time.Time{}, "requeued", now)
}

// AutoRetry queues the automatic retry of a job that failed transiently: same repo, next
// attempt, not claimed before notBefore, continuing the failed job's WIP when it has one.
// countAttempt=false (worker shutdown) keeps the attempt number.
func (s *Store) AutoRetry(ctx context.Context, failed Job, countAttempt bool, notBefore, now time.Time) (Job, error) {
	attempt := failed.Attempt
	if countAttempt {
		attempt++
	}
	return s.enqueue(ctx, failed.JiraKey, failed.RepoName, attempt, notBefore, "auto_retry", now)
}

func (s *Store) enqueue(ctx context.Context, key, repo string, attempt int, notBefore time.Time, event string, now time.Time) (Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()

	var st State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE jira_key = ?`, key).Scan(&st); err != nil {
		return Job{}, err
	}
	if st != StateApproved {
		return Job{}, ErrInvalidTransition
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE jira_key = ? AND status IN (?, ?)`,
		key, JobQueued, JobRunning).Scan(&active); err != nil {
		return Job{}, err
	}
	if active > 0 {
		return Job{}, ErrJobActive
	}
	var resumeOf sql.NullInt64
	last, err := scanJob(tx.QueryRowContext(ctx, selectJob+` WHERE jira_key = ? ORDER BY id DESC LIMIT 1`, key))
	switch {
	case err == nil && last.RepoName == repo && last.Status == JobFailed && last.WIPSHA != "":
		resumeOf = sql.NullInt64{Int64: last.ID, Valid: true}
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return Job{}, err
	}
	var nb sql.NullInt64
	if !notBefore.IsZero() {
		nb = sql.NullInt64{Int64: notBefore.Unix(), Valid: true}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO jobs (jira_key, repo_name, status, created_at, resume_of, attempt, not_before)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, key, repo, JobQueued, now.Unix(), resumeOf, attempt, nb)
	if err != nil {
		return Job{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Job{}, err
	}
	if err := addEvent(ctx, tx, key, event, repo, now); err != nil {
		return Job{}, err
	}
	j, err := scanJob(tx.QueryRowContext(ctx, selectJob+` WHERE id = ?`, id))
	if err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}
