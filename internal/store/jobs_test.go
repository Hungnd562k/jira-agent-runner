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

func TestAutoRetryBackoffAndResume(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	s.Upsert(ctx, IssueInput{Key: "X-1", Summary: "s", URL: "u"}, now)
	s.MarkNotified(ctx, "X-1", 1, now)
	_, first, _ := s.ApproveAndEnqueue(ctx, "X-1", "api", now)
	s.ClaimJob(ctx, now)
	if err := s.SetJobAgent(ctx, first.ID, 4242, "boot"); err != nil {
		t.Fatal(err)
	}
	failed, err := s.FinishJob(ctx, first.ID, JobResult{Status: JobFailed, Error: "net", WIPSHA: "abc"}, now)
	if err != nil || failed.WIPSHA != "abc" || failed.AgentPGID != 0 {
		t.Fatalf("finish = %+v %v", failed, err)
	}

	retry, err := s.AutoRetry(ctx, failed, true, now.Add(time.Minute), now)
	if err != nil || retry.Attempt != 1 || retry.ResumeOf != first.ID || !retry.NotBefore.Equal(now.Add(time.Minute)) {
		t.Fatalf("retry = %+v %v", retry, err)
	}
	if _, err := s.AutoRetry(ctx, failed, true, now, now); !errors.Is(err, ErrJobActive) {
		t.Fatalf("second retry while queued: %v", err)
	}
	if _, ok, _ := s.ClaimJob(ctx, now.Add(30*time.Second)); ok {
		t.Fatal("claimed before not_before")
	}
	if j, ok, _ := s.ClaimJob(ctx, now.Add(time.Minute)); !ok || j.ID != retry.ID {
		t.Fatalf("claim at not_before = %+v %v", j, ok)
	}
	s.FinishJob(ctx, retry.ID, JobResult{Status: JobFailed, Error: "x"}, now)

	// No WIP on the latest failure, or another repo: a fresh start.
	if j, _ := s.Enqueue(ctx, "X-1", "api", now); j.ResumeOf != 0 || j.Attempt != 0 {
		t.Fatalf("manual retry without wip = %+v", j)
	}
}

func TestJobGroups(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	for _, k := range []string{"X-1", "X-2"} {
		s.Upsert(ctx, IssueInput{Key: k, Summary: k, URL: k}, now)
		s.MarkNotified(ctx, k, 1, now)
	}
	_, jobs, err := s.ApproveAndEnqueueRepos(ctx, "X-1", []string{"api", "web"}, now)
	if err != nil || len(jobs) != 2 || jobs[0].GroupID != jobs[0].ID || jobs[1].GroupID != jobs[0].ID {
		t.Fatalf("group = %+v %v", jobs, err)
	}
	s.ApproveAndEnqueue(ctx, "X-2", "api", now)

	claimed, ok, err := s.ClaimGroup(ctx, now)
	if err != nil || !ok || len(claimed) != 2 || claimed[0].Status != JobRunning || claimed[1].RepoName != "web" {
		t.Fatalf("claim = %+v %v %v", claimed, ok, err)
	}
	if g, _ := s.LatestGroup(ctx, "X-1"); len(g) != 2 {
		t.Fatalf("latest group = %+v", g)
	}
	if _, err := s.LatestGroup(ctx, "NOPE"); !IsNotFound(err) {
		t.Fatalf("latest group of unknown task: %v", err)
	}
	s.FinishJob(ctx, claimed[0].ID, JobResult{Status: JobFailed, Error: "connection reset", WIPSHA: "w1"}, now)
	s.FinishJob(ctx, claimed[1].ID, JobResult{Status: JobFailed, Error: "connection reset"}, now)

	// The auto-retry is one new group; only the repo with WIP resumes.
	retry, err := s.AutoRetryGroup(ctx, []Job{claimed[0], claimed[1]}, true, time.Time{}, now)
	if err != nil || len(retry) != 2 || retry[0].GroupID == claimed[0].GroupID || retry[1].GroupID != retry[0].GroupID ||
		retry[0].ResumeOf != claimed[0].ID || retry[1].ResumeOf != 0 || retry[0].Attempt != 1 {
		t.Fatalf("retry = %+v %v", retry, err)
	}
	// The X-2 job was queued first: it is claimed alone before the retry group.
	if g, ok, _ := s.ClaimGroup(ctx, now); !ok || len(g) != 1 || g[0].JiraKey != "X-2" {
		t.Fatalf("claim X-2 = %+v", g)
	}
	if g, ok, _ := s.ClaimGroup(ctx, now); !ok || len(g) != 2 || g[0].JiraKey != "X-1" {
		t.Fatalf("claim retry = %+v", g)
	}
}
