package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// ErrInvalidTransition means the task is not in a state that allows the requested change.
var ErrInvalidTransition = errors.New("invalid state transition")

// transition atomically moves key to `to` if its current state is in `from`,
// applying extra SET clauses, and records an event. Returns the updated task.
func (s *Store) transition(ctx context.Context, key string, from []State, to State,
	set string, args []any, event, payload string, now time.Time) (Task, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()

	var cur State
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE jira_key = ?`, key).Scan(&cur); err != nil {
		return Task{}, err
	}
	if !slices.Contains(from, cur) {
		return Task{}, ErrInvalidTransition
	}

	q := `UPDATE tasks SET state = ?, updated_at = ?`
	if set != "" {
		q += ", " + set
	}
	q += ` WHERE jira_key = ?`
	all := append([]any{to, now.Unix()}, args...)
	all = append(all, key)
	if _, err := tx.ExecContext(ctx, q, all...); err != nil {
		return Task{}, err
	}
	if err := addEvent(ctx, tx, key, event, payload, now); err != nil {
		return Task{}, err
	}

	t, err := scanTask(tx.QueryRowContext(ctx, selectTask+` WHERE jira_key = ?`, key))
	if err != nil {
		return Task{}, err
	}
	return t, tx.Commit()
}

// MarkNotified records that a notice (or reminder) was sent as messageID.
func (s *Store) MarkNotified(ctx context.Context, key string, messageID int64, now time.Time) (Task, error) {
	return s.transition(ctx, key, []State{StateNew, StateNotified, StateSnoozed}, StateNotified,
		"tg_message_id = ?, last_notified_at = ?, remind_at = NULL", []any{messageID, now.Unix()},
		"notified", "", now)
}

// Approve marks a task approved ("Fix it").
func (s *Store) Approve(ctx context.Context, key string, now time.Time) (Task, error) {
	return s.transition(ctx, key, []State{StateNew, StateNotified, StateSnoozed}, StateApproved,
		"remind_at = NULL", nil, "approved", "", now)
}

// Snooze postpones the next reminder until `until`.
func (s *Store) Snooze(ctx context.Context, key string, until, now time.Time) (Task, error) {
	return s.transition(ctx, key, []State{StateNotified, StateSnoozed}, StateSnoozed,
		"remind_at = ?", []any{until.Unix()}, "snoozed", until.UTC().Format(time.RFC3339), now)
}

// IsNotFound reports whether err means the task does not exist.
func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// IsDue reports whether t needs a reminder at now: a notified task untouched for
// remindAfter, or a snoozed task whose snooze has expired.
func IsDue(t Task, now time.Time, remindAfter time.Duration) bool {
	switch t.State {
	case StateNotified:
		return !t.LastNotifiedAt.IsZero() && !now.Before(t.LastNotifiedAt.Add(remindAfter))
	case StateSnoozed:
		return !t.RemindAt.IsZero() && !now.Before(t.RemindAt)
	}
	return false
}

// ListDue returns tasks that need a reminder at now (see IsDue).
func (s *Store) ListDue(ctx context.Context, now time.Time, remindAfter time.Duration) ([]Task, error) {
	open, err := s.ListByState(ctx, StateNotified, StateSnoozed)
	if err != nil {
		return nil, err
	}
	var due []Task
	for _, t := range open {
		if IsDue(t, now, remindAfter) {
			due = append(due, t)
		}
	}
	return due, nil
}
