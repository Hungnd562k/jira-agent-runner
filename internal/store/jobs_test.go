package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestJobLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	for _, k := range []string{"X-1", "X-2"} {
		if _, err := s.Upsert(ctx, IssueInput{Key: k, Summary: k, URL: k}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkNotified(ctx, k, 1, now); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok, err := s.ClaimJob(ctx, now); ok || err != nil {
		t.Fatalf("claim on empty queue: ok=%v err=%v", ok, err)
	}

	task, job, err := s.ApproveAndEnqueue(ctx, "X-1", "api", now)
	if err != nil || task.State != StateApproved || job.Status != JobQueued || job.RepoName != "api" {
		t.Fatalf("enqueue = %+v %+v %v", task, job, err)
	}
	if _, _, err := s.ApproveAndEnqueue(ctx, "X-1", "api", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double enqueue: %v", err)
	}
	if _, _, err := s.ApproveAndEnqueue(ctx, "X-2", "web", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.ClaimJob(ctx, now)
	if err != nil || !ok || got.ID != job.ID || got.Status != JobRunning || got.StartedAt.IsZero() {
		t.Fatalf("claim = %+v ok=%v err=%v (want oldest first)", got, ok, err)
	}
	if err := s.SetJobBranch(ctx, got.ID, "agent/X-1-fix"); err != nil {
		t.Fatal(err)
	}

	passed := true
	done, err := s.FinishJob(ctx, got.ID, JobResult{Status: JobSucceeded, MRURL: "https://mr/1", TestsPassed: &passed}, now)
	if err != nil || done.Status != JobSucceeded || done.Branch != "agent/X-1-fix" || done.TestsPassed == nil || !*done.TestsPassed {
		t.Fatalf("finish = %+v %v", done, err)
	}
	latest, err := s.LatestJob(ctx, "X-1")
	if err != nil || latest.MRURL != "https://mr/1" {
		t.Fatalf("latest = %+v %v", latest, err)
	}
	if _, err := s.LatestJob(ctx, "NOPE"); !IsNotFound(err) {
		t.Fatalf("latest missing: %v", err)
	}

	// Second job is claimed, then the worker "crashes".
	if _, ok, _ := s.ClaimJob(ctx, now); !ok {
		t.Fatal("second claim failed")
	}
	orphans, err := s.FailOrphaned(ctx, now)
	if err != nil || len(orphans) != 1 || orphans[0].JiraKey != "X-2" || orphans[0].Status != JobFailed {
		t.Fatalf("orphans = %+v %v", orphans, err)
	}
}

func TestEnqueueRetry(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.Upsert(ctx, IssueInput{Key: "X-1", Summary: "s", URL: "u"}, now)
	s.MarkNotified(ctx, "X-1", 1, now)

	if _, err := s.Enqueue(ctx, "X-1", "api", now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("enqueue before approval: %v", err)
	}
	_, first, _ := s.ApproveAndEnqueue(ctx, "X-1", "api", now)
	if _, err := s.Enqueue(ctx, "X-1", "api", now); !errors.Is(err, ErrJobActive) {
		t.Fatalf("enqueue while queued: %v", err)
	}
	s.ClaimJob(ctx, now)
	s.FinishJob(ctx, first.ID, JobResult{Status: JobFailed, Error: "x"}, now)

	j, err := s.Enqueue(ctx, "X-1", "web", now)
	if err != nil || j.ID == first.ID || j.RepoName != "web" || j.Status != JobQueued {
		t.Fatalf("retry = %+v %v", j, err)
	}
	if latest, _ := s.LatestJob(ctx, "X-1"); latest.ID != j.ID {
		t.Fatalf("latest = %+v", latest)
	}
}
