package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	in := IssueInput{Key: "X-1", Summary: "bug", Priority: "High", Status: "Open", URL: "u", Updated: now}

	mustUpsert := func(want UpsertResult) {
		t.Helper()
		got, err := s.Upsert(ctx, in, now)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if got != want {
			t.Fatalf("Upsert = %v, want %v", got, want)
		}
	}

	mustUpsert(Created)
	mustUpsert(Unchanged)

	in.Summary = "bug v2"
	mustUpsert(Updated)

	task, err := s.Get(ctx, "X-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != StateNew || task.Summary != "bug v2" || !task.JiraUpdated.Equal(now) {
		t.Errorf("unexpected task: %+v", task)
	}

	closed, err := s.CloseMissing(ctx, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].JiraKey != "X-1" || closed[0].State != StateNew {
		t.Fatalf("CloseMissing = %+v", closed)
	}
	if task, _ = s.Get(ctx, "X-1"); task.State != StateClosed {
		t.Errorf("state after close = %s", task.State)
	}

	mustUpsert(Reopened)
	if task, _ = s.Get(ctx, "X-1"); task.State != StateNew {
		t.Errorf("state after reopen = %s", task.State)
	}
}

func TestCloseMissingKeepsSeen(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()
	for _, k := range []string{"A-1", "A-2"} {
		if _, err := s.Upsert(ctx, IssueInput{Key: k, Summary: k, URL: k}, now); err != nil {
			t.Fatal(err)
		}
	}
	closed, err := s.CloseMissing(ctx, []string{"A-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].JiraKey != "A-2" {
		t.Fatalf("closed = %+v", closed)
	}
	open, _ := s.ListByState(ctx, StateNew)
	if len(open) != 1 || open[0].JiraKey != "A-1" {
		t.Fatalf("open = %+v", open)
	}
}

func TestOpenFileReappliesNoMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "tasks.db")
	for range 2 { // second open must not re-run 001_init.sql
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		s.Close()
	}
}
