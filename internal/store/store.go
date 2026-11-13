// Package store persists task state in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// State is a task's lifecycle state. See docs/SPEC.md.
type State string

const (
	StateNew      State = "new"
	StateNotified State = "notified"
	StateSnoozed  State = "snoozed"
	StateApproved State = "approved"
	StateClosed   State = "closed"
)

// openStates are states whose tasks are still tracked against Jira.
var openStates = []State{StateNew, StateNotified, StateSnoozed, StateApproved}

// Task is a tracked Jira issue.
type Task struct {
	JiraKey        string
	Summary        string
	Priority       string
	JiraStatus     string
	URL            string
	State          State
	TGMessageID    int64 // 0 = not sent yet
	LastNotifiedAt time.Time
	RemindAt       time.Time
	JiraUpdated    time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// IssueInput is the Jira-side data used to upsert a task.
type IssueInput struct {
	Key, Summary, Priority, Status, URL string
	Updated                             time.Time
}

// UpsertResult tells the caller what an upsert did.
type UpsertResult int

const (
	Unchanged UpsertResult = iota
	Created
	Updated
	Reopened // was closed, matches JQL again
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at path and applies migrations.
// Use ":memory:" for tests.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := ":memory:"
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; also keeps :memory: on one connection
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies migrations/NNN_*.sql in order, tracked via PRAGMA user_version.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(files)
	for i, f := range files {
		n := i + 1
		if n <= version {
			continue
		}
		body, err := migrationsFS.ReadFile(f)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", f, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Upsert inserts a new task (state=new) or refreshes Jira fields on an existing one.
// A closed task that shows up again is reopened (state=new, message reset).
func (s *Store) Upsert(ctx context.Context, in IssueInput, now time.Time) (UpsertResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Unchanged, err
	}
	defer tx.Rollback()

	var (
		state                             State
		summary, priority, status, rawURL string
		jiraUpdated                       sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT state, summary, priority, jira_status, url, jira_updated FROM tasks WHERE jira_key = ?`, in.Key).
		Scan(&state, &summary, &priority, &status, &rawURL, &jiraUpdated)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO tasks
			(jira_key, summary, priority, jira_status, url, state, jira_updated, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Key, in.Summary, in.Priority, in.Status, in.URL, StateNew, unixOrNull(in.Updated), now.Unix(), now.Unix())
		if err != nil {
			return Unchanged, err
		}
		if err := addEvent(ctx, tx, in.Key, "created", "", now); err != nil {
			return Unchanged, err
		}
		return Created, tx.Commit()

	case err != nil:
		return Unchanged, err

	case state == StateClosed:
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET summary=?, priority=?, jira_status=?, url=?, jira_updated=?,
			state=?, tg_message_id=NULL, last_notified_at=NULL, remind_at=NULL, updated_at=? WHERE jira_key=?`,
			in.Summary, in.Priority, in.Status, in.URL, unixOrNull(in.Updated), StateNew, now.Unix(), in.Key)
		if err != nil {
			return Unchanged, err
		}
		if err := addEvent(ctx, tx, in.Key, "reopened", "", now); err != nil {
			return Unchanged, err
		}
		return Reopened, tx.Commit()
	}

	if summary == in.Summary && priority == in.Priority && status == in.Status && rawURL == in.URL &&
		jiraUpdated == unixOrNull(in.Updated) {
		return Unchanged, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET summary=?, priority=?, jira_status=?, url=?, jira_updated=?, updated_at=?
		WHERE jira_key=?`, in.Summary, in.Priority, in.Status, in.URL, unixOrNull(in.Updated), now.Unix(), in.Key)
	if err != nil {
		return Unchanged, err
	}
	return Updated, tx.Commit()
}

// CloseMissing marks every open task whose key is not in seen as closed and returns them
// (with their pre-close state and message id) so the caller can update notifications.
func (s *Store) CloseMissing(ctx context.Context, seen []string, now time.Time) ([]Task, error) {
	open, err := s.ListByState(ctx, openStates...)
	if err != nil {
		return nil, err
	}
	seenSet := make(map[string]struct{}, len(seen))
	for _, k := range seen {
		seenSet[k] = struct{}{}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var closed []Task
	for _, t := range open {
		if _, ok := seenSet[t.JiraKey]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET state=?, remind_at=NULL, updated_at=? WHERE jira_key=?`,
			StateClosed, now.Unix(), t.JiraKey); err != nil {
			return nil, err
		}
		if err := addEvent(ctx, tx, t.JiraKey, "closed", string(t.State), now); err != nil {
			return nil, err
		}
		closed = append(closed, t)
	}
	return closed, tx.Commit()
}

// Get returns one task, or sql.ErrNoRows.
func (s *Store) Get(ctx context.Context, key string) (Task, error) {
	row := s.db.QueryRowContext(ctx, selectTask+` WHERE jira_key = ?`, key)
	return scanTask(row)
}

// ListByState returns tasks in any of the given states, ordered by key.
func (s *Store) ListByState(ctx context.Context, states ...State) ([]Task, error) {
	if len(states) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := make([]any, len(states))
	for i, st := range states {
		args[i] = st
	}
	rows, err := s.db.QueryContext(ctx, selectTask+` WHERE state IN (`+ph+`) ORDER BY jira_key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const selectTask = `SELECT jira_key, summary, priority, jira_status, url, state,
	tg_message_id, last_notified_at, remind_at, jira_updated, created_at, updated_at FROM tasks`

type scanner interface{ Scan(dest ...any) error }

func scanTask(r scanner) (Task, error) {
	var (
		t                                Task
		msgID, notified, remind, jiraUpd sql.NullInt64
		created, updated                 int64
	)
	if err := r.Scan(&t.JiraKey, &t.Summary, &t.Priority, &t.JiraStatus, &t.URL, &t.State,
		&msgID, &notified, &remind, &jiraUpd, &created, &updated); err != nil {
		return Task{}, err
	}
	t.TGMessageID = msgID.Int64
	t.LastNotifiedAt = fromNull(notified)
	t.RemindAt = fromNull(remind)
	t.JiraUpdated = fromNull(jiraUpd)
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	return t, nil
}

func addEvent(ctx context.Context, tx *sql.Tx, key, typ, payload string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (jira_key, type, payload, created_at) VALUES (?, ?, ?, ?)`,
		key, typ, payload, now.Unix())
	return err
}

func unixOrNull(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func fromNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0)
}
