package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTransitions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	if _, err := s.Upsert(ctx, IssueInput{Key: "X-1", Summary: "s", URL: "u"}, now); err != nil {
		t.Fatal(err)
	}

	// Snooze not allowed before the notice is sent.
	if _, err := s.Snooze(ctx, "X-1", now.Add(time.Hour), now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("snooze from new: %v", err)
	}

	task, err := s.MarkNotified(ctx, "X-1", 42, now)
	if err != nil || task.State != StateNotified || task.TGMessageID != 42 || !task.LastNotifiedAt.Equal(now) {
		t.Fatalf("MarkNotified = %+v, %v", task, err)
	}

	until := now.Add(30 * time.Minute)
	task, err = s.Snooze(ctx, "X-1", until, now)
	if err != nil || task.State != StateSnoozed || !task.RemindAt.Equal(until) {
		t.Fatalf("Snooze = %+v, %v", task, err)
	}

	task, err = s.Approve(ctx, "X-1", now)
	if err != nil || task.State != StateApproved || !task.RemindAt.IsZero() || task.TGMessageID != 42 {
		t.Fatalf("Approve = %+v, %v", task, err)
	}

	// Double press on "Fix it" is rejected, not re-applied.
	if _, err := s.Approve(ctx, "X-1", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second approve: %v", err)
	}
	if _, err := s.Approve(ctx, "NOPE-1", now); !IsNotFound(err) {
		t.Fatalf("approve missing: %v", err)
	}
}
