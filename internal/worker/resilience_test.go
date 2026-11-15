package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

func TestIsTransient(t *testing.T) {
	for msg, want := range map[string]bool{
		"agent error (error_during_execution): API Error: Connection error.":           true,
		`agent error (error_during_execution): API Error: 529 {"type":"overloaded"}`:   true,
		"agent error (x): API Error: 503 Service Unavailable":                          true,
		"prepare worktree: git fetch: exit status 128: Could not resolve host: gitlab": true,
		"load issue: Get \"https://jira\": dial tcp: lookup jira: i/o timeout":         true,
		"push: git network step timed out: git push: signal: killed":                   true,
		"interrupted (worker restarted)":                                               true,
		"agent timed out after 30m0s":                                                  false,
		"agent made no changes":                                                        false,
		"agent error (error_max_budget_usd): budget exceeded":                          false,
		"repo \"x\" not found in repos.json":                                           false,
	} {
		if got := isTransient(msg); got != want {
			t.Errorf("isTransient(%q) = %v, want %v", msg, got, want)
		}
	}
}

// flakyClaude fails with a network error after a partial edit on its first run; the second run
// requires that partial edit to be back in the working tree, then finishes the fix.
func flakyClaude(dir string) string {
	return `if [ ! -f ` + dir + `/ran ]; then
  touch ` + dir + `/ran
  echo half > app.txt
  echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: Connection error."}'
  exit 1
fi
grep -q half app.txt || { echo '{"type":"result","subtype":"success","is_error":true,"result":"WIP missing"}'; exit 1; }
echo fixed > app.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","total_cost_usd":0.1,"num_turns":2}'
`
}

func TestTransientFailureKeepsWIPAndResumes(t *testing.T) {
	ctx := context.Background()
	w, tg, clone, _ := setup(t, flakyClaude(t.TempDir()))
	w.NoPush = true
	w.AutoRetries, w.RetryDelay = 2, time.Minute
	now := w.Now()

	if did, err := w.RunOnce(ctx); err != nil || !did {
		t.Fatalf("first run = %v %v", did, err)
	}
	first, _ := w.Store.GetJob(ctx, 1)
	if first.Status != store.JobFailed || first.WIPSHA == "" {
		t.Fatalf("first job = %+v", first)
	}
	if out := git(t, clone, "show", first.WIPSHA+":app.txt"); out != "half" {
		t.Errorf("wip content = %q", out)
	}
	retry, _ := w.Store.LatestJob(ctx, "X-1")
	if retry.ID == first.ID || retry.Status != store.JobQueued || retry.ResumeOf != first.ID || retry.Attempt != 1 ||
		!retry.NotBefore.Equal(now.Add(time.Minute)) {
		t.Fatalf("retry = %+v", retry)
	}
	if last := tg.edits[len(tg.edits)-1]; !strings.Contains(last, "Auto-retry #1") {
		t.Errorf("notice = %q", last)
	}
	if len(tg.sends) == 0 || !strings.Contains(tg.sends[len(tg.sends)-1], "Transient failure") {
		t.Errorf("sends = %v", tg.sends)
	}

	if did, _ := w.RunOnce(ctx); did {
		t.Fatal("retry claimed before its backoff")
	}
	w.Now = func() time.Time { return now.Add(2 * time.Minute) }
	if did, err := w.RunOnce(ctx); err != nil || !did {
		t.Fatalf("retry run = %v %v", did, err)
	}
	done, _ := w.Store.GetJob(ctx, retry.ID)
	if done.Status != store.JobSucceeded || done.WIPSHA != "" {
		t.Fatalf("retry job = %+v", done)
	}
	if out := git(t, clone, "show", done.Branch+":app.txt"); out != "fixed" {
		t.Errorf("fixed content = %q", out)
	}
	if n := git(t, clone, "rev-list", "--count", "origin/main.."+done.Branch); n != "1" {
		t.Errorf("retry branch should hold one clean fix commit, has %s", n)
	}
}

func TestFinalFailureKeepsWIPWithoutRetry(t *testing.T) {
	ctx := context.Background()
	w, tg, clone, _ := setup(t, "echo half > app.txt\nsleep 30\n")
	w.NoPush = true
	w.AutoRetries = 3
	w.Claude.Timeout = 300 * time.Millisecond

	w.RunOnce(ctx)
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.ID != 1 || job.Status != store.JobFailed || job.WIPSHA == "" {
		t.Fatalf("timeout is not transient; job = %+v", job)
	}
	if msg := git(t, clone, "log", "-1", "--format=%s", job.Branch); !strings.HasPrefix(msg, "wip(X-1)") {
		t.Errorf("wip commit = %q", msg)
	}
	if len(tg.sends) == 0 || !strings.Contains(tg.sends[len(tg.sends)-1], "Partial work kept") {
		t.Errorf("sends = %v", tg.sends)
	}
	// A manual retry on the same repo continues from the WIP.
	next, err := w.Store.Enqueue(ctx, "X-1", "app", w.Now())
	if err != nil || next.ResumeOf != job.ID {
		t.Fatalf("manual retry = %+v %v", next, err)
	}
	w.MR = &fakeMR{}
	if err := w.Publish(ctx, job.ID); err == nil || !strings.Contains(err.Error(), "WIP") {
		t.Errorf("publish of WIP-only job = %v", err)
	}
}

func TestRecoverReapsAgentSalvagesAndRetries(t *testing.T) {
	ctx := context.Background()
	w, tg, clone, _ := setup(t, okClaude)
	w.AutoRetries, w.BootID = 1, "boot-1"
	job, ok, _ := w.Store.ClaimJob(ctx, w.Now())
	if !ok {
		t.Fatal("claim")
	}
	// The crashed worker left a worktree with edits and an agent still running.
	root, name := worktreeLocation(w.WorktreeRoot, clone, job)
	wt, err := agent.NewWorktree(ctx, clone, root, name, "agent/X-1-j1", "main")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt.Dir, "app.txt"), []byte("half\n"), 0o644)
	w.Store.SetJobBranch(ctx, job.ID, "agent/X-1-j1")
	orphan := exec.Command("sleep", "60")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { orphan.Wait(); close(exited) }()
	w.Store.SetJobAgent(ctx, job.ID, orphan.Process.Pid, "boot-1")

	if err := w.recover(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		orphan.Process.Kill()
		t.Fatal("orphaned agent not reaped")
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Errorf("stale worktree still on disk: %v", err)
	}
	failed, _ := w.Store.GetJob(ctx, job.ID)
	if failed.Status != store.JobFailed || failed.WIPSHA == "" {
		t.Fatalf("orphan = %+v", failed)
	}
	retry, _ := w.Store.LatestJob(ctx, "X-1")
	if retry.ID == job.ID || retry.ResumeOf != job.ID || retry.Attempt != 1 {
		t.Fatalf("retry = %+v", retry)
	}
	if last := tg.edits[len(tg.edits)-1]; !strings.Contains(last, "Auto-retry") {
		t.Errorf("notice = %q", last)
	}
}

func TestRecoverIgnoresPGIDFromAnotherBoot(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := setup(t, okClaude)
	w.BootID = "boot-2"
	job, _, _ := w.Store.ClaimJob(ctx, w.Now())
	bystander := exec.Command("sleep", "60")
	bystander.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer bystander.Process.Kill()
	w.Store.SetJobAgent(ctx, job.ID, bystander.Process.Pid, "boot-1")

	if err := w.recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process from another boot was killed: %v", err)
	}
}
