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
}

const selectJob = `SELECT id, jira_key, repo_name, status, branch, mr_url, error, tests_passed,
	created_at, started_at, finished_at FROM jobs`

func scanJob(r scanner) (Job, error) {
	var (
		j                 Job
		passed            sql.NullInt64
		created           int64
		started, finished sql.NullInt64
	)
	if err := r.Scan(&j.ID, &j.JiraKey, &j.RepoName, &j.Status, &j.Branch, &j.MRURL, &j.Error, &passed,
		&created, &started, &finished); err != nil {
		return Job{}, err
	}
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

// ClaimJob atomically moves the oldest queued job to running. ok=false when the queue is empty.
func (s *Store) ClaimJob(ctx context.Context, now time.Time) (Job, bool, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, `UPDATE jobs SET status = ?, started_at = ?
		WHERE id = (SELECT id FROM jobs WHERE status = ? ORDER BY id LIMIT 1)
		RETURNING id, jira_key, repo_name, status, branch, mr_url, error, tests_passed,
		created_at, started_at, finished_at`, JobRunning, now.Unix(), JobQueued))
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
		WHERE status = ? RETURNING id, jira_key, repo_name, status, branch, mr_url, error, tests_passed,
		created_at, started_at, finished_at`, JobFailed, now.Unix(), JobRunning)
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
	j, err := scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET status = ?, mr_url = ?, error = ?, tests_passed = ?, finished_at = ?
		WHERE id = ? RETURNING id, jira_key, repo_name, status, branch, mr_url, error, tests_passed,
		created_at, started_at, finished_at`, r.Status, r.MRURL, r.Error, passed, now.Unix(), id))
	if err != nil {
		return Job{}, err
	}
	payload := r.MRURL
	if r.Status == JobFailed {
		payload = r.Error
	}
	if err := addEvent(ctx, tx, j.JiraKey, "job_"+string(r.Status), payload, now); err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}
