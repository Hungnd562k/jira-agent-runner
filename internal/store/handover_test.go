package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// mergedJob creates an approved task with a job finished and merged into dev.
func mergedJob(t *testing.T, s *Store, key, repo string, now time.Time) Job {
	t.Helper()
	ctx := context.Background()
	s.Upsert(ctx, IssueInput{Key: key, Summary: key, URL: key}, now)
	s.MarkNotified(ctx, key, 1, now)
	if _, _, err := s.ApproveAndEnqueue(ctx, key, repo, now); err != nil {
		t.Fatal(err)
	}
	j, _, _ := s.ClaimJob(ctx, now)
	j, err := s.FinishJob(ctx, j.ID, JobResult{Status: JobSucceeded, MRURL: "mr", MRIID: 5, MergedSHA: "abc"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestHandoverLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)

	j1 := mergedJob(t, s, "X-1", "web", now)
	if j1.MergedAt.IsZero() || j1.MergedSHA != "abc" || j1.MRIID != 5 {
		t.Fatalf("merged job = %+v", j1)
	}
	pending, err := s.PendingHandoverJobs(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v %v", pending, err)
	}

	h, err := s.ProposeHandover(ctx, []int64{j1.ID}, []HandoverRepo{{RepoName: "web", Project: "g/web"}}, now)
	if err != nil || h.Status != HandoverProposed || len(h.Jobs) != 1 || len(h.Repos) != 1 {
		t.Fatalf("propose = %+v %v", h, err)
	}
	if _, ok, _ := s.BlockingHandover(ctx, now); !ok {
		t.Error("proposed handover should block a new proposal")
	}

	// Later: jobs released, blocked until remind_at.
	if h, err = s.DecideHandover(ctx, h.ID, false, now.Add(time.Hour), now); err != nil || h.Status != HandoverSnoozed || len(h.Jobs) != 0 {
		t.Fatalf("snooze = %+v %v", h, err)
	}
	if _, err := s.DecideHandover(ctx, h.ID, true, time.Time{}, now); !errors.Is(err, ErrHandoverState) {
		t.Fatalf("decide twice: %v", err)
	}
	if _, ok, _ := s.BlockingHandover(ctx, now.Add(30*time.Minute)); !ok {
		t.Error("snoozed handover should block until remind_at")
	}
	if _, ok, _ := s.BlockingHandover(ctx, now.Add(2*time.Hour)); ok {
		t.Error("expired snooze should not block")
	}

	// New proposal, yes, a second ticket merged meanwhile is attached at execution.
	h2, _ := s.ProposeHandover(ctx, []int64{j1.ID}, []HandoverRepo{{RepoName: "web", Project: "g/web"}}, now)
	if _, err := s.DecideHandover(ctx, h2.ID, true, time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	mergedJob(t, s, "X-2", "web", now)
	got, ok, err := s.NextApprovedHandover(ctx)
	if err != nil || !ok || got.ID != h2.ID {
		t.Fatalf("next approved = %+v %v %v", got, ok, err)
	}
	if err := s.AttachPendingJobs(ctx, h2.ID, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	r := got.Repos[0]
	r.Status, r.MergeSHA, r.MRIID = HRMerged, "def", 9
	if err := s.UpdateHandoverRepo(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHandoverStatus(ctx, h2.ID, HandoverDeploying, "", now.Add(time.Hour), false, now); err != nil {
		t.Fatal(err)
	}
	dep, err := s.HandoversIn(ctx, HandoverDeploying)
	if err != nil || len(dep) != 1 || len(dep[0].Jobs) != 2 || dep[0].Repos[0].MergeSHA != "def" || dep[0].DeadlineAt.IsZero() {
		t.Fatalf("deploying = %+v %v", dep, err)
	}
	if err := s.SetHandoverStatus(ctx, h2.ID, HandoverSucceeded, "", time.Time{}, false, now); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.GetJob(ctx, j1.ID); j.DeliveredAt.IsZero() {
		t.Error("delivered_at not set")
	}
	if p, _ := s.PendingHandoverJobs(ctx); len(p) != 0 {
		t.Errorf("pending after delivery = %v", p)
	}
}

func TestHandoverFailureReleasesJobs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	j := mergedJob(t, s, "X-1", "web", now)
	h, _ := s.ProposeHandover(ctx, []int64{j.ID}, []HandoverRepo{{RepoName: "web", Project: "g/web"}}, now)
	s.DecideHandover(ctx, h.ID, true, time.Time{}, now)
	if err := s.SetHandoverStatus(ctx, h.ID, HandoverFailed, "merge refused", time.Time{}, true, now); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PendingHandoverJobs(ctx); len(p) != 1 {
		t.Fatalf("job not released: %v", p)
	}
	if n, _ := s.ActiveJobCount(ctx); n != 0 {
		t.Errorf("active = %d", n)
	}
}
