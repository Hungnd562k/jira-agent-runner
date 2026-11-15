package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
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
	GroupID     int64     // jobs queued together (one per repo of the scope) share it; = ID of the first
}

// jobColumns is shared by every SELECT/RETURNING so scanJob always matches.
const jobColumns = `id, jira_key, repo_name, status, branch, mr_url, error, tests_passed,
	created_at, started_at, finished_at, mr_iid, merged_sha, merged_at, handover_id, delivered_at,
	wip_sha, resume_of, attempt, not_before, agent_pgid, boot_id, COALESCE(group_id, id)`

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
		&j.WIPSHA, &resumeOf, &j.Attempt, &notBef, &j.AgentPGID, &j.BootID, &j.GroupID); err != nil {
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
	t, jobs, err := s.ApproveAndEnqueueRepos(ctx, key, []string{repo}, now)
	if err != nil {
		return Task{}, Job{}, err
	}
	return t, jobs[0], nil
}

// ApproveAndEnqueueRepos approves the task and queues one job group covering repos (one job
// per repo, run by a single agent session), atomically.
func (s *Store) ApproveAndEnqueueRepos(ctx context.Context, key string, repoNames []string, now time.Time) (Task, []Job, error) {
	if len(repoNames) == 0 {
		return Task{}, nil, errors.New("no repo to queue")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, nil, err
	}
	defer tx.Rollback()

	t, err := transitionTx(ctx, tx, key, []State{StateNew, StateNotified, StateSnoozed}, StateApproved,
		"remind_at = NULL", nil, "approved", strings.Join(repoNames, ","), now)
	if err != nil {
		return Task{}, nil, err
	}
	jobs, err := insertGroup(ctx, tx, key, repoNames, 0, time.Time{}, now)
	if err != nil {
		return Task{}, nil, err
	}
	return t, jobs, tx.Commit()
}

// insertGroup queues one job per repo sharing a group id. A repo whose latest job failed with
// partial work (WIP) continues from it. The caller checks the task state.
func insertGroup(ctx context.Context, tx *sql.Tx, key string, repoNames []string, attempt int, notBefore, now time.Time) ([]Job, error) {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE jira_key = ? AND status IN (?, ?)`,
		key, JobQueued, JobRunning).Scan(&active); err != nil {
		return nil, err
	}
	if active > 0 {
		return nil, ErrJobActive
	}
	var nb sql.NullInt64
	if !notBefore.IsZero() {
		nb = sql.NullInt64{Int64: notBefore.Unix(), Valid: true}
	}
	var (
		group int64
		ids   []int64
	)
	for _, repo := range repoNames {
		var resumeOf sql.NullInt64
		last, err := scanJob(tx.QueryRowContext(ctx, selectJob+` WHERE jira_key = ? AND repo_name = ? ORDER BY id DESC LIMIT 1`, key, repo))
		switch {
		case err == nil && last.Status == JobFailed && last.WIPSHA != "":
			resumeOf = sql.NullInt64{Int64: last.ID, Valid: true}
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		var g sql.NullInt64
		if group != 0 {
			g = sql.NullInt64{Int64: group, Valid: true}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO jobs (jira_key, repo_name, status, created_at, resume_of, attempt, not_before, group_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, key, repo, JobQueued, now.Unix(), resumeOf, attempt, nb, g)
		if err != nil {
			return nil, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		if group == 0 {
			group = id
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET group_id = ? WHERE id = ?`, id, id); err != nil {
				return nil, err
			}
		}
		ids = append(ids, id)
	}
	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		j, err := scanJob(tx.QueryRowContext(ctx, selectJob+` WHERE id = ?`, id))
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// LatestJob returns the most recent job for a task, or sql.ErrNoRows.
func (s *Store) LatestJob(ctx context.Context, key string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, selectJob+` WHERE jira_key = ? ORDER BY id DESC LIMIT 1`, key))
}

// GetJob returns a job by id.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, selectJob+` WHERE id = ?`, id))
}

// LatestGroup returns the jobs of the task's most recent group (ordered by id), or
// sql.ErrNoRows when the task has no job.
func (s *Store) LatestGroup(ctx context.Context, key string) ([]Job, error) {
	last, err := s.LatestJob(ctx, key)
	if err != nil {
		return nil, err
	}
	return s.Group(ctx, last.GroupID)
}

// Group returns the jobs of a group, ordered by id.
func (s *Store) Group(ctx context.Context, groupID int64) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, selectJob+` WHERE COALESCE(group_id, id) = ? ORDER BY id`, groupID)
	if err != nil {
		return nil, err
	}
	return collectJobs(rows)
}

func collectJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

// ClaimJob claims the next due group (see ClaimGroup) and returns its first job.
func (s *Store) ClaimJob(ctx context.Context, now time.Time) (Job, bool, error) {
	jobs, ok, err := s.ClaimGroup(ctx, now)
	if err != nil || !ok {
		return Job{}, ok, err
	}
	return jobs[0], true, nil
}

// ClaimGroup atomically moves the oldest due queued job and the rest of its group to running
// (jobs waiting for a retry backoff are skipped until not_before). ok=false when nothing is due.
func (s *Store) ClaimGroup(ctx context.Context, now time.Time) ([]Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var group int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(group_id, id) FROM jobs
		WHERE status = ? AND (not_before IS NULL OR not_before <= ?) ORDER BY id LIMIT 1`, JobQueued, now.Unix()).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE jobs SET status = ?, started_at = ?
		WHERE status = ? AND COALESCE(group_id, id) = ? RETURNING `+jobColumns, JobRunning, now.Unix(), JobQueued, group)
	if err != nil {
		return nil, false, err
	}
	jobs, err := collectJobs(rows)
	if err != nil {
		return nil, false, err
	}
	return jobs, len(jobs) > 0, tx.Commit()
}

// FailOrphaned marks jobs left running (e.g. worker crashed) as failed. Call at worker start.
func (s *Store) FailOrphaned(ctx context.Context, now time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE jobs SET status = ?, error = 'interrupted (worker restarted)', finished_at = ?
		WHERE status = ? RETURNING `+jobColumns, JobFailed, now.Unix(), JobRunning)
	if err != nil {
		return nil, err
	}
	return collectJobs(rows)
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

// Enqueue queues another job (manual retry) for an already approved task.
// A retry on the same repo continues the partial work (WIP) of its latest failed job.
func (s *Store) Enqueue(ctx context.Context, key, repo string, now time.Time) (Job, error) {
	jobs, err := s.EnqueueRepos(ctx, key, []string{repo}, now)
	if err != nil {
		return Job{}, err
	}
	return jobs[0], nil
}

// EnqueueRepos queues a new job group over repos for an already approved task.
func (s *Store) EnqueueRepos(ctx context.Context, key string, repoNames []string, now time.Time) ([]Job, error) {
	return s.enqueue(ctx, key, repoNames, 0, time.Time{}, "requeued", now)
}

// AutoRetry queues the automatic retry of a job that failed transiently (see AutoRetryGroup).
func (s *Store) AutoRetry(ctx context.Context, failed Job, countAttempt bool, notBefore, now time.Time) (Job, error) {
	jobs, err := s.AutoRetryGroup(ctx, []Job{failed}, countAttempt, notBefore, now)
	if err != nil {
		return Job{}, err
	}
	return jobs[0], nil
}

// AutoRetryGroup queues the automatic retry of jobs that failed transiently, as one new group:
// same repos, next attempt, not claimed before notBefore, each continuing its job's WIP when it
// has one. countAttempt=false (worker shutdown) keeps the attempt number.
func (s *Store) AutoRetryGroup(ctx context.Context, failed []Job, countAttempt bool, notBefore, now time.Time) ([]Job, error) {
	if len(failed) == 0 {
		return nil, errors.New("no job to retry")
	}
	attempt := 0
	names := make([]string, 0, len(failed))
	for _, j := range failed {
		attempt = max(attempt, j.Attempt)
		names = append(names, j.RepoName)
	}
	if countAttempt {
		attempt++
	}
	return s.enqueue(ctx, failed[0].JiraKey, names, attempt, notBefore, "auto_retry", now)
}

func (s *Store) enqueue(ctx context.Context, key string, repoNames []string, attempt int, notBefore time.Time, event string, now time.Time) ([]Job, error) {
	if len(repoNames) == 0 {
		return nil, errors.New("no repo to queue")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var st State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE jira_key = ?`, key).Scan(&st); err != nil {
		return nil, err
	}
	if st != StateApproved {
		return nil, ErrInvalidTransition
	}
	jobs, err := insertGroup(ctx, tx, key, repoNames, attempt, notBefore, now)
	if err != nil {
		return nil, err
	}
	if err := addEvent(ctx, tx, key, event, strings.Join(repoNames, ","), now); err != nil {
		return nil, err
	}
	return jobs, tx.Commit()
}
