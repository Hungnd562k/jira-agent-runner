package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

type fakeResolver struct{ transitioned, assigned []string }

func (f *fakeResolver) Transition(_ context.Context, key, to, _ string) (string, error) {
	f.transitioned = append(f.transitioned, key)
	return to, nil
}
func (f *fakeResolver) AssignToReporter(_ context.Context, key string) (string, error) {
	f.assigned = append(f.assigned, key)
	return "QC An", nil
}

// mergeSetup: a repo with auto_merge + handover to "test", push mode, fakes for GitLab/Jira.
func mergeSetup(t *testing.T) (*Worker, *fakeTG, *fakeMR, *fakeComment, *fakeResolver, string) {
	t.Helper()
	w, tg, clone, _ := setup(t, okClaude)
	w.Repos["X"][0].AutoMerge = true
	w.Repos["X"][0].HandoverBranch = "test"
	mr, jc, res := &fakeMR{}, &fakeComment{}, &fakeResolver{}
	w.MR, w.Comment, w.Resolver = mr, jc, res
	return w, tg, mr, jc, res, clone
}

func TestAutoMergeIntoDev(t *testing.T) {
	ctx := context.Background()
	w, tg, mr, jc, _, _ := mergeSetup(t)

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobSucceeded || job.MergedAt.IsZero() || job.MergedSHA != "sha1" || job.MRIID != 1 {
		t.Fatalf("job = %+v", job)
	}
	if mr.req.Draft || len(mr.approved) != 1 || len(mr.merged) != 1 {
		t.Fatalf("mr = %+v approved=%v merged=%v", mr.req, mr.approved, mr.merged)
	}
	if !strings.Contains(tg.sends[0], "đã merge vào <code>main</code>") || !strings.Contains(tg.edits[len(tg.edits)-1], "chờ bàn giao") {
		t.Errorf("telegram = %v / %v", tg.sends, tg.edits)
	}
	if !strings.Contains(jc.body, "*chờ bàn giao*") || !strings.Contains(jc.body, "Co-authored-by") {
		t.Errorf("jira comment = %s", jc.body)
	}
}

func TestMergeGateKeepsDraft(t *testing.T) {
	ctx := context.Background()
	w, tg, mr, _, _, _ := mergeSetup(t)
	w.Repos["X"][0].E2E = &repos.E2E{BaseURL: "http://localhost:8803", CredentialsProfile: "p"} // agent leaves no evidence
	w.CredentialsFile = filepath.Join(t.TempDir(), "none.json")

	w.RunOnce(ctx)
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if !job.MergedAt.IsZero() || !mr.req.Draft || len(mr.merged) != 0 {
		t.Fatalf("gate should keep a Draft MR: job=%+v mr=%+v", job, mr.req)
	}
	if !strings.Contains(tg.sends[0], "chưa merge (E2E not complete)") {
		t.Errorf("report = %s", tg.sends[0])
	}
}

func TestAutoMergeFailureLeavesMROpen(t *testing.T) {
	ctx := context.Background()
	w, tg, mr, _, _, _ := mergeSetup(t)
	mr.mergeErr = errors.New("gitlab merge MR !1: conflicts with the target branch")

	w.RunOnce(ctx)
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobSucceeded || !job.MergedAt.IsZero() || job.MRURL == "" {
		t.Fatalf("job = %+v", job)
	}
	if !strings.Contains(tg.sends[0], "merge failed: gitlab merge MR !1: conflicts") {
		t.Errorf("report = %s", tg.sends[0])
	}
}

func TestHandoverLifecycle(t *testing.T) {
	ctx := context.Background()
	w, tg, mr, jc, res, clone := mergeSetup(t)
	w.RunOnce(ctx) // X-1 fixed and merged into main (dev)

	// Another developer's commit sits in main but not in test.
	os.WriteFile(filepath.Join(clone, "other.txt"), []byte("x"), 0o644)
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "add", "other.txt")
	git(t, clone, "commit", "-q", "-m", "feat: other dev work")
	git(t, clone, "push", "-q", "origin", "HEAD:main")

	// Busy queue → no question.
	w.Store.Upsert(ctx, store.IssueInput{Key: "X-2", Summary: "s", URL: "u"}, w.Now())
	w.Store.MarkNotified(ctx, "X-2", 8, w.Now())
	w.Store.ApproveAndEnqueue(ctx, "X-2", "app", w.Now())
	sent := len(tg.sends)
	if err := w.proposeHandover(ctx); err != nil || len(tg.sends) != sent {
		t.Fatalf("proposed while a job is queued: %v %v", err, tg.sends[sent:])
	}
	j2, _, _ := w.Store.ClaimJob(ctx, w.Now())
	w.Store.FinishJob(ctx, j2.ID, store.JobResult{Status: store.JobFailed, Error: "x"}, w.Now())

	// Idle → question listing the ticket and the foreign commit.
	if err := w.proposeHandover(ctx); err != nil {
		t.Fatal(err)
	}
	q := tg.sends[len(tg.sends)-1]
	if !strings.Contains(q, "Bàn giao lên test?") || !strings.Contains(q, "X-1") || !strings.Contains(q, "1 commit khác") ||
		!strings.Contains(q, "feat: other dev work") || strings.Contains(q, "fix(X-1)") {
		t.Fatalf("question = %s", q)
	}
	if err := w.proposeHandover(ctx); err != nil || tg.sends[len(tg.sends)-1] != q {
		t.Fatal("second proposal while one is open")
	}

	// "Yes" (notifier) → worker opens main→test, approves, merges, watches CI/CD.
	h, ok, _ := w.Store.BlockingHandover(ctx, w.Now())
	if !ok {
		t.Fatal("no handover")
	}
	if _, err := w.Store.DecideHandover(ctx, h.ID, true, time.Time{}, w.Now()); err != nil {
		t.Fatal(err)
	}
	w.runApprovedHandover(ctx)
	ho := mr.reqs[len(mr.reqs)-1]
	if ho.SourceBranch != "main" || ho.Target != "test" || !ho.KeepSource || ho.Draft || !strings.Contains(ho.Title, "X-1") {
		t.Fatalf("handover MR = %+v", ho)
	}
	if !strings.Contains(tg.sends[len(tg.sends)-1], "Đã merge sang test") {
		t.Fatalf("merge message = %s", tg.sends[len(tg.sends)-1])
	}

	// Pipeline running → still deploying; success → tickets delivered.
	mr.pipeline = &Pipeline{ID: 42, Status: "running", WebURL: "https://gitlab/p/42"}
	w.WatchOnce(ctx)
	if got, _ := w.Store.GetHandover(ctx, h.ID); got.Status != store.HandoverDeploying || got.Repos[0].PipelineID != 42 {
		t.Fatalf("running: %+v", got)
	}
	mr.pipeline.Status = "success"
	w.WatchOnce(ctx)
	got, _ := w.Store.GetHandover(ctx, h.ID)
	if got.Status != store.HandoverSucceeded {
		t.Fatalf("success: %+v", got)
	}
	msg := tg.sends[len(tg.sends)-1]
	if !strings.Contains(msg, "CI/CD test thành công") || !strings.Contains(msg, "X-1</a> Crash on save — Resolved, giao lại cho QC An") ||
		!strings.Contains(msg, "pipeline #42") {
		t.Fatalf("success message = %s", msg)
	}
	if strings.Join(res.transitioned, ",") != "X-1" || strings.Join(res.assigned, ",") != "X-1" ||
		!strings.Contains(jc.body, "Đã bàn giao lên {{test}}") {
		t.Errorf("jira: transitioned=%v assigned=%v comment=%s", res.transitioned, res.assigned, jc.body)
	}
	if j, _ := w.Store.GetJob(ctx, 1); j.DeliveredAt.IsZero() {
		t.Error("job not delivered")
	}
	if !strings.Contains(tg.edits[len(tg.edits)-1], "Đã bàn giao test") {
		t.Errorf("notice = %s", tg.edits[len(tg.edits)-1])
	}
	if err := w.proposeHandover(ctx); err != nil || tg.sends[len(tg.sends)-1] != msg {
		t.Error("nothing should be proposed after delivery")
	}
}

func TestHandoverPipelineFailure(t *testing.T) {
	ctx := context.Background()
	w, tg, mr, _, res, _ := mergeSetup(t)
	w.RunOnce(ctx)
	w.proposeHandover(ctx)
	h, _, _ := w.Store.BlockingHandover(ctx, w.Now())
	w.Store.DecideHandover(ctx, h.ID, true, time.Time{}, w.Now())
	w.runApprovedHandover(ctx)

	mr.pipeline = &Pipeline{ID: 42, Status: "failed", WebURL: "https://gitlab/p/42"}
	mr.failed = []PipelineJob{{ID: 9, Name: "build", Stage: "build", FailureReason: "script_failure"}}
	w.WatchOnce(ctx)
	got, _ := w.Store.GetHandover(ctx, h.ID)
	if got.Status != store.HandoverFailed || len(res.transitioned) != 0 {
		t.Fatalf("handover = %+v transitioned=%v", got, res.transitioned)
	}
	msg := tg.sends[len(tg.sends)-1]
	if !strings.Contains(msg, "CI/CD test thất bại") || !strings.Contains(msg, "job build (stage build) failed: script_failure") ||
		!strings.Contains(msg, "CS1002") {
		t.Fatalf("failure message = %s", msg)
	}

	// User retries the pipeline in GitLab and presses "watch again".
	if _, err := w.Store.RewatchHandover(ctx, h.ID, w.Now().Add(time.Hour), w.Now()); err != nil {
		t.Fatal(err)
	}
	mr.pipeline.Status = "success"
	w.WatchOnce(ctx)
	if got, _ := w.Store.GetHandover(ctx, h.ID); got.Status != store.HandoverSucceeded || len(res.transitioned) != 1 {
		t.Fatalf("after rewatch: %+v", got)
	}
}

func TestHandoverMergeFailureReleasesTickets(t *testing.T) {
	ctx := context.Background()
	w, tg, mr, _, _, _ := mergeSetup(t)
	w.RunOnce(ctx)
	w.proposeHandover(ctx)
	h, _, _ := w.Store.BlockingHandover(ctx, w.Now())
	w.Store.DecideHandover(ctx, h.ID, true, time.Time{}, w.Now())
	mr.mergeErr = errors.New("gitlab merge MR: HTTP 405: Method Not Allowed (merge status: not_approved)")

	w.runApprovedHandover(ctx)
	if got, _ := w.Store.GetHandover(ctx, h.ID); got.Status != store.HandoverFailed {
		t.Fatalf("handover = %+v", got)
	}
	if !strings.Contains(tg.sends[len(tg.sends)-1], "not_approved") {
		t.Errorf("message = %s", tg.sends[len(tg.sends)-1])
	}
	if p, _ := w.Store.PendingHandoverJobs(ctx); len(p) != 1 {
		t.Errorf("ticket should wait for the next proposal: %v", p)
	}
}
