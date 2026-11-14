package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// HandoverStatus is the lifecycle of a dev → test handover. See docs/SPEC.md.
type HandoverStatus string

const (
	HandoverProposed  HandoverStatus = "proposed"  // question sent on Telegram
	HandoverSnoozed   HandoverStatus = "snoozed"   // "later": jobs released, ask again after remind_at
	HandoverApproved  HandoverStatus = "approved"  // "yes": worker creates/approves/merges the MRs
	HandoverDeploying HandoverStatus = "deploying" // merged, watching CI/CD
	HandoverSucceeded HandoverStatus = "succeeded"
	HandoverFailed    HandoverStatus = "failed"
)

// Handover-repo statuses.
const (
	HRPending = "pending"
	HRMerged  = "merged"
	HRSuccess = "success"
	HRFailed  = "failed"
)

// Handover batches tickets merged into dev for one dev → test merge per repo.
type Handover struct {
	ID          int64
	Status      HandoverStatus
	TGMessageID int64
	Message     string // Telegram HTML of the question
	RemindAt    time.Time
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeadlineAt  time.Time
	Repos       []HandoverRepo
	Jobs        []Job
}

// HandoverRepo is the dev → test MR and CI/CD pipeline of one repo in a handover.
type HandoverRepo struct {
	HandoverID  int64
	RepoName    string
	Project     string
	MRIID       int64
	MRURL       string
	MergeSHA    string
	PipelineID  int64
	PipelineURL string
	Status      string
	Error       string
}

// ErrHandoverState means the handover is no longer in the expected status (e.g. double click).
var ErrHandoverState = errors.New("handover is not in the expected state")

const pendingJobs = `merged_at IS NOT NULL AND handover_id IS NULL AND delivered_at IS NULL`

// PendingHandoverJobs lists jobs merged into dev that no handover carries yet, oldest first.
func (s *Store) PendingHandoverJobs(ctx context.Context) ([]Job, error) {
	return s.queryJobs(ctx, selectJob+` WHERE `+pendingJobs+` ORDER BY id`)
}

// ActiveJobCount counts queued or running jobs.
func (s *Store) ActiveJobCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN (?, ?)`, JobQueued, JobRunning).Scan(&n)
	return n, err
}

// BlockingHandover returns the handover that prevents a new proposal: one in flight
// (proposed/approved/deploying) or snoozed until after now.
func (s *Store) BlockingHandover(ctx context.Context, now time.Time) (Handover, bool, error) {
	h, err := s.scanHandover(s.db.QueryRowContext(ctx, selectHandover+` WHERE status IN (?, ?, ?)
		OR (status = ? AND remind_at > ?) ORDER BY id DESC LIMIT 1`,
		HandoverProposed, HandoverApproved, HandoverDeploying, HandoverSnoozed, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return Handover{}, false, nil
	}
	return h, err == nil, err
}

// ProposeHandover creates a proposed handover carrying jobIDs (only those still pending).
func (s *Store) ProposeHandover(ctx context.Context, jobIDs []int64, repos []HandoverRepo, now time.Time) (Handover, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Handover{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO handovers (status, created_at, updated_at) VALUES (?, ?, ?)`,
		HandoverProposed, now.Unix(), now.Unix())
	if err != nil {
		return Handover{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Handover{}, err
	}
	for _, jid := range jobIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET handover_id = ? WHERE id = ? AND `+pendingJobs, id, jid); err != nil {
			return Handover{}, err
		}
	}
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT INTO handover_repos (handover_id, repo_name, project) VALUES (?, ?, ?)`,
			id, r.RepoName, r.Project); err != nil {
			return Handover{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Handover{}, err
	}
	return s.GetHandover(ctx, id)
}

// AttachPendingJobs adds jobs merged after the proposal (their commits ride along in the merge).
func (s *Store) AttachPendingJobs(ctx context.Context, id int64, repoNames []string) error {
	for _, r := range repoNames {
		if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET handover_id = ? WHERE repo_name = ? AND `+pendingJobs, id, r); err != nil {
			return err
		}
	}
	return nil
}

// DetachRepoJobs releases a repo's jobs from a handover (its dev → test merge failed),
// so they wait for the next proposal instead of being reported as delivered.
func (s *Store) DetachRepoJobs(ctx context.Context, id int64, repoName string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET handover_id = NULL WHERE handover_id = ? AND repo_name = ?`, id, repoName)
	return err
}

// SetHandoverMessage records the Telegram message that asks the question and its HTML.
func (s *Store) SetHandoverMessage(ctx context.Context, id, messageID int64, html string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE handovers SET tg_message_id = ?, message = ? WHERE id = ?`, messageID, html, id)
	return err
}

// RewatchHandover puts a failed handover whose MRs were merged back to watching CI/CD
// (after the user retried the pipeline in GitLab).
func (s *Store) RewatchHandover(ctx context.Context, id int64, deadline, now time.Time) (Handover, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Handover{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE handovers SET status = ?, error = '', deadline_at = ?, updated_at = ?
		WHERE id = ? AND status = ? AND EXISTS (SELECT 1 FROM handover_repos WHERE handover_id = ? AND merge_sha <> '')`,
		HandoverDeploying, deadline.Unix(), now.Unix(), id, HandoverFailed, id)
	if err != nil {
		return Handover{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Handover{}, ErrHandoverState
	}
	if _, err := tx.ExecContext(ctx, `UPDATE handover_repos SET status = ?, error = '' WHERE handover_id = ? AND status = ? AND merge_sha <> ''`,
		HRMerged, id, HRFailed); err != nil {
		return Handover{}, err
	}
	if err := tx.Commit(); err != nil {
		return Handover{}, err
	}
	return s.GetHandover(ctx, id)
}

// DecideHandover applies the user's answer to a proposed handover: approve, or snooze until
// remindAt (jobs are released so the next proposal recomputes the batch).
func (s *Store) DecideHandover(ctx context.Context, id int64, approve bool, remindAt, now time.Time) (Handover, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Handover{}, err
	}
	defer tx.Rollback()
	status, remind := HandoverApproved, sql.NullInt64{}
	if !approve {
		status, remind = HandoverSnoozed, sql.NullInt64{Int64: remindAt.Unix(), Valid: true}
	}
	res, err := tx.ExecContext(ctx, `UPDATE handovers SET status = ?, remind_at = ?, updated_at = ? WHERE id = ? AND status = ?`,
		status, remind, now.Unix(), id, HandoverProposed)
	if err != nil {
		return Handover{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Handover{}, ErrHandoverState
	}
	if !approve {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET handover_id = NULL WHERE handover_id = ?`, id); err != nil {
			return Handover{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Handover{}, err
	}
	return s.GetHandover(ctx, id)
}

// NextApprovedHandover returns the oldest approved handover waiting for the worker.
func (s *Store) NextApprovedHandover(ctx context.Context) (Handover, bool, error) {
	h, err := s.scanHandover(s.db.QueryRowContext(ctx, selectHandover+` WHERE status = ? ORDER BY id LIMIT 1`, HandoverApproved))
	if errors.Is(err, sql.ErrNoRows) {
		return Handover{}, false, nil
	}
	if err != nil {
		return Handover{}, false, err
	}
	h, err = s.GetHandover(ctx, h.ID)
	return h, err == nil, err
}

// HandoversIn lists handovers in a status, oldest first, with repos and jobs.
func (s *Store) HandoversIn(ctx context.Context, status HandoverStatus) ([]Handover, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM handovers WHERE status = ? ORDER BY id`, status)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Handover, 0, len(ids))
	for _, id := range ids {
		h, err := s.GetHandover(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// UpdateHandoverRepo stores a repo's MR / pipeline progress.
func (s *Store) UpdateHandoverRepo(ctx context.Context, r HandoverRepo) error {
	_, err := s.db.ExecContext(ctx, `UPDATE handover_repos SET mr_iid = ?, mr_url = ?, merge_sha = ?, pipeline_id = ?,
		pipeline_url = ?, status = ?, error = ? WHERE handover_id = ? AND repo_name = ?`,
		r.MRIID, r.MRURL, r.MergeSHA, r.PipelineID, r.PipelineURL, r.Status, r.Error, r.HandoverID, r.RepoName)
	return err
}

// SetHandoverStatus moves a handover to status. Failing before anything was merged releases
// its jobs (release=true) so a later proposal picks them up again.
func (s *Store) SetHandoverStatus(ctx context.Context, id int64, status HandoverStatus, errMsg string, deadline time.Time, release bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE handovers SET status = ?, error = ?, deadline_at = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, toNull(deadline), now.Unix(), id); err != nil {
		return err
	}
	if release {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET handover_id = NULL WHERE handover_id = ?`, id); err != nil {
			return err
		}
	}
	if status == HandoverSucceeded {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET delivered_at = ? WHERE handover_id = ?`, now.Unix(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const selectHandover = `SELECT id, status, tg_message_id, message, remind_at, error, created_at, updated_at, deadline_at FROM handovers`

func (s *Store) scanHandover(r scanner) (Handover, error) {
	var (
		h                Handover
		remind, deadline sql.NullInt64
		created, updated int64
	)
	if err := r.Scan(&h.ID, &h.Status, &h.TGMessageID, &h.Message, &remind, &h.Error, &created, &updated, &deadline); err != nil {
		return Handover{}, err
	}
	h.RemindAt, h.DeadlineAt = fromNull(remind), fromNull(deadline)
	h.CreatedAt, h.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return h, nil
}

// GetHandover returns a handover with its repos and jobs.
func (s *Store) GetHandover(ctx context.Context, id int64) (Handover, error) {
	h, err := s.scanHandover(s.db.QueryRowContext(ctx, selectHandover+` WHERE id = ?`, id))
	if err != nil {
		return Handover{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT handover_id, repo_name, project, mr_iid, mr_url, merge_sha, pipeline_id,
		pipeline_url, status, error FROM handover_repos WHERE handover_id = ? ORDER BY repo_name`, id)
	if err != nil {
		return Handover{}, err
	}
	for rows.Next() {
		var r HandoverRepo
		if err := rows.Scan(&r.HandoverID, &r.RepoName, &r.Project, &r.MRIID, &r.MRURL, &r.MergeSHA, &r.PipelineID,
			&r.PipelineURL, &r.Status, &r.Error); err != nil {
			rows.Close()
			return Handover{}, err
		}
		h.Repos = append(h.Repos, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Handover{}, err
	}
	h.Jobs, err = s.queryJobs(ctx, selectJob+` WHERE handover_id = ? ORDER BY id`, id)
	return h, err
}

func (s *Store) queryJobs(ctx context.Context, q string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
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

func toNull(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}
