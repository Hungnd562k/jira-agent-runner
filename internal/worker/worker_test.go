package worker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"
)

type fakeJira struct{}

func (fakeJira) GetIssue(_ context.Context, key string) (jira.IssueDetail, error) {
	return jira.IssueDetail{Issue: jira.Issue{Key: key, Summary: "Crash on save", URL: "https://jira/browse/" + key},
		Type: "Bug", Description: "Saving crashes"}, nil
}

type fakeTG struct {
	edits []string
	sends []string
}

func (f *fakeTG) SendMessage(_ context.Context, _ int64, html string, _ telegram.Keyboard, _ int64) (int64, error) {
	f.sends = append(f.sends, html)
	return 99, nil
}
func (f *fakeTG) EditMessage(_ context.Context, _, _ int64, html string, _ telegram.Keyboard) error {
	f.edits = append(f.edits, html)
	return nil
}

type fakeMR struct{ req *MRRequest }

func (f *fakeMR) OpenDraftMR(_ context.Context, r MRRequest) (string, error) {
	f.req = &r
	return "https://gitlab/mr/1", nil
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := agent.Git{Dir: dir}.Run(context.Background(), args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// setup creates origin (bare) + a clone with one commit on main, a fake claude script,
// and a store with one approved task + queued job.
func setup(t *testing.T, claudeScript string) (*Worker, *fakeTG, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e", "GIT_COMMITTER_NAME": "t",
		"GIT_COMMITTER_EMAIL": "t@e", "GIT_CONFIG_GLOBAL": "/dev/null"} {
		t.Setenv(k, v)
	}
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	clone := filepath.Join(root, "clone")
	git(t, root, "init", "--bare", "-b", "main", origin)
	git(t, root, "clone", origin, clone)
	os.WriteFile(filepath.Join(clone, "app.txt"), []byte("buggy\n"), 0o644)
	git(t, clone, "add", "-A")
	git(t, clone, "commit", "-m", "init")
	git(t, clone, "push", "origin", "HEAD:main")

	bin := filepath.Join(root, "fake-claude.sh")
	os.WriteFile(bin, []byte("#!/bin/sh\ncat > /dev/null\n"+claudeScript), 0o755)

	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Unix(1_800_000_000, 0)
	st.Upsert(ctx, store.IssueInput{Key: "X-1", Summary: "Crash on save", URL: "u"}, now)
	st.MarkNotified(ctx, "X-1", 7, now)
	if _, _, err := st.ApproveAndEnqueue(ctx, "X-1", "app", now); err != nil {
		t.Fatal(err)
	}

	tg := &fakeTG{}
	w := &Worker{
		Store: st,
		Repos: repos.Config{"X": {{Name: "app", GitLabProject: "g/app", Path: clone, BaseBranch: "main",
			TestCmd: "grep -q fixed app.txt"}}},
		Jira:         fakeJira{},
		TG:           tg,
		ChatID:       1,
		Claude:       agent.ClaudeSpec{Bin: bin, Timeout: time.Minute},
		TestTimeout:  time.Minute,
		WorktreeRoot: filepath.Join(root, "wt"),
		JobsDir:      filepath.Join(root, "jobs"),
		Now:          func() time.Time { return now },
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return w, tg, clone, origin
}

const okClaude = `echo fixed > app.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"### Root cause\nbug","total_cost_usd":0.12,"num_turns":3}'
`

func TestPipelineLocalCommit(t *testing.T) {
	ctx := context.Background()
	w, tg, clone, _ := setup(t, okClaude)
	w.NoPush = true

	did, err := w.RunOnce(ctx)
	if err != nil || !did {
		t.Fatalf("RunOnce = %v %v", did, err)
	}
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobSucceeded || job.Branch != "agent/X-1-j1" || job.TestsPassed == nil || !*job.TestsPassed {
		t.Fatalf("job = %+v", job)
	}
	if msg := git(t, clone, "log", "-1", "--format=%B", job.Branch); !strings.HasPrefix(msg, "fix(X-1): Crash on save") ||
		!strings.Contains(msg, "Co-Authored-By: Claude") {
		t.Errorf("commit message = %q", msg)
	}
	if out := git(t, clone, "worktree", "list"); strings.Contains(out, "X-1-j1") {
		t.Errorf("worktree not removed: %s", out)
	}
	if len(tg.edits) < 2 || !strings.Contains(tg.edits[0], "Agent running") || !strings.Contains(tg.edits[len(tg.edits)-1], "not pushed") {
		t.Errorf("edits = %v", tg.edits)
	}
	if _, err := os.Stat(filepath.Join(w.JobsDir, "1", "test.log")); err != nil {
		t.Errorf("test log missing: %v", err)
	}
	if did, _ := w.RunOnce(ctx); did {
		t.Error("queue should be empty")
	}
}

func TestPipelinePushAndMR(t *testing.T) {
	ctx := context.Background()
	w, _, _, origin := setup(t, okClaude)
	mr := &fakeMR{}
	w.MR = mr

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobSucceeded || job.MRURL != "https://gitlab/mr/1" {
		t.Fatalf("job = %+v", job)
	}
	if out := git(t, origin, "branch", "--list", "agent/X-1-j1"); out == "" {
		t.Error("branch not pushed to origin")
	}
	if mr.req == nil || mr.req.Title != "Draft: [X-1] Crash on save" || !strings.Contains(mr.req.Description, "✅") ||
		!strings.Contains(mr.req.Description, "Root cause") {
		t.Fatalf("mr request = %+v", mr.req)
	}
}

func TestPipelineNoChangesFails(t *testing.T) {
	ctx := context.Background()
	w, tg, _, _ := setup(t, `echo '{"type":"result","subtype":"success","is_error":false,"result":"Not a code bug in this repo"}'`+"\n")
	w.NoPush = true

	w.RunOnce(ctx)
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobFailed || job.Error != "agent made no changes" {
		t.Fatalf("job = %+v", job)
	}
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "Not a code bug") {
		t.Errorf("failure detail not sent: %v", tg.sends)
	}
}

func TestPipelineAgentErrorAndTimeout(t *testing.T) {
	ctx := context.Background()
	w, _, _, _ := setup(t, "sleep 30\n")
	w.NoPush = true
	w.Claude.Timeout = 300 * time.Millisecond

	start := time.Now()
	w.RunOnce(ctx)
	if time.Since(start) > 15*time.Second {
		t.Fatal("timeout did not kill the agent")
	}
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "timed out") {
		t.Fatalf("job = %+v", job)
	}
}

func TestRecoverOrphans(t *testing.T) {
	ctx := context.Background()
	w, tg, _, _ := setup(t, okClaude)
	if _, ok, _ := w.Store.ClaimJob(ctx, w.Now()); !ok {
		t.Fatal("claim")
	}
	if err := w.recover(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobFailed || !strings.Contains(tg.edits[len(tg.edits)-1], "interrupted") {
		t.Fatalf("job = %+v edits = %v", job, tg.edits)
	}
}

func TestWorktreeNextToRepoSeesSiblingsAndIgnoredGuidance(t *testing.T) {
	ctx := context.Background()
	// The fake agent fails unless the ignored CLAUDE.md was copied and ../sibling resolves.
	w, _, clone, _ := setup(t, "test -f CLAUDE.md || exit 3\ntest -f ../sibling/marker || exit 4\n"+okClaude)
	w.NoPush = true
	w.WorktreeRoot = "" // default: next to the clone

	root := filepath.Dir(clone)
	os.MkdirAll(filepath.Join(root, "sibling"), 0o755)
	os.WriteFile(filepath.Join(root, "sibling", "marker"), nil, 0o644)
	os.WriteFile(filepath.Join(clone, ".gitignore"), []byte("CLAUDE.md\n"), 0o644)
	git(t, clone, "add", ".gitignore")
	git(t, clone, "commit", "-m", "ignore")
	git(t, clone, "push", "origin", "HEAD:main")
	os.WriteFile(filepath.Join(clone, "CLAUDE.md"), []byte("rules"), 0o644)

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobSucceeded {
		t.Fatalf("job = %+v", job)
	}
	if files := git(t, clone, "show", "--name-only", "--format=", job.Branch); strings.Contains(files, "CLAUDE.md") {
		t.Errorf("ignored CLAUDE.md was committed: %s", files)
	}
	if _, err := os.Stat(filepath.Join(root, ".agent-clone-X-1-j1")); !os.IsNotExist(err) {
		t.Errorf("sibling worktree not removed: %v", err)
	}
}

type fakeComment struct{ key, body string }

func (f *fakeComment) AddComment(_ context.Context, key, body string) error {
	f.key, f.body = key, body
	return nil
}

func TestReportPublished(t *testing.T) {
	ctx := context.Background()

	// Local (no push): Telegram report tells where the branch is; no Jira comment.
	w, tg, clone, _ := setup(t, okClaude)
	w.NoPush = true
	jc := &fakeComment{}
	w.Comment = jc
	w.RunOnce(ctx)
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "git -C "+clone+" show agent/X-1-j1") ||
		!strings.Contains(tg.sends[0], "Root cause") || !strings.Contains(tg.sends[0], "app.txt") {
		t.Fatalf("local report = %v", tg.sends)
	}
	if jc.key != "" {
		t.Errorf("Jira commented without an MR: %q", jc.body)
	}
	md, err := os.ReadFile(filepath.Join(w.JobsDir, "1", "report.md"))
	if err != nil || !strings.Contains(string(md), "Branch: `agent/X-1-j1`") {
		t.Fatalf("report.md = %q %v", md, err)
	}

	// Push: report links the MR and the Jira issue gets a comment.
	w2, tg2, _, _ := setup(t, okClaude)
	w2.MR = &fakeMR{}
	jc2 := &fakeComment{}
	w2.Comment = jc2
	w2.RunOnce(ctx)
	if len(tg2.sends) != 1 || !strings.Contains(tg2.sends[0], "https://gitlab/mr/1") {
		t.Fatalf("push report = %v", tg2.sends)
	}
	if jc2.key != "X-1" || !strings.Contains(jc2.body, "https://gitlab/mr/1") || !strings.Contains(jc2.body, "(/)") {
		t.Fatalf("jira comment = %+v", jc2)
	}
}
