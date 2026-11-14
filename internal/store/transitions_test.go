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

func TestListDue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	t0 := time.Unix(1_800_000_000, 0)
	for _, k := range []string{"N-1", "S-1", "A-1", "NEW-1"} {
		if _, err := s.Upsert(ctx, IssueInput{Key: k, Summary: k, URL: k}, t0); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"N-1", "S-1", "A-1"} {
		if _, err := s.MarkNotified(ctx, k, 1, t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Snooze(ctx, "S-1", t0.Add(30*time.Minute), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, "A-1", t0); err != nil {
		t.Fatal(err)
	}

	keys := func(now time.Time) []string {
		due, err := s.ListDue(ctx, now, 4*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, d := range due {
			out = append(out, d.JiraKey)
		}
		return out
	}
	if got := keys(t0.Add(10 * time.Minute)); len(got) != 0 {
		t.Errorf("t+10m due = %v", got)
	}
	if got := keys(t0.Add(30 * time.Minute)); len(got) != 1 || got[0] != "S-1" {
		t.Errorf("t+30m due = %v", got)
	}
	if got := keys(t0.Add(4 * time.Hour)); len(got) != 2 {
		t.Errorf("t+4h due = %v (approved/new must never be due)", got)
	}
}
