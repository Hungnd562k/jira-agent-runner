package worker

import (
	"context"
	"errors"
	"fmt"
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

// fakeMR is an in-memory GitLab: MRs get iid 1, 2, …; pipeline is what FindPipeline/GetPipeline return.
type fakeMR struct {
	req      *MRRequest
	reqs     []MRRequest
	err      error // OpenMR error
	mergeErr error
	approved []int64
	merged   []int64
	pipeline *Pipeline
	failed   []PipelineJob
}

func (f *fakeMR) OpenMR(_ context.Context, r MRRequest) (MR, error) {
	f.req = &r
	if f.err != nil {
		return MR{}, f.err
	}
	f.reqs = append(f.reqs, r)
	n := int64(len(f.reqs))
	return MR{IID: n, WebURL: fmt.Sprintf("https://gitlab/mr/%d", n)}, nil
}
func (f *fakeMR) FindOpenMR(context.Context, string, string, string) (MR, bool, error) {
	return MR{}, false, nil
}
func (f *fakeMR) ApproveMR(_ context.Context, _ string, iid int64) error {
	f.approved = append(f.approved, iid)
	return nil
}
func (f *fakeMR) MergeMR(_ context.Context, _ string, iid int64, _ bool) (string, error) {
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	f.merged = append(f.merged, iid)
	return fmt.Sprintf("sha%d", iid), nil
}
func (f *fakeMR) FindPipeline(context.Context, string, string, string) (Pipeline, bool, error) {
	if f.pipeline == nil {
		return Pipeline{}, false, nil
	}
	return *f.pipeline, true, nil
}
func (f *fakeMR) GetPipeline(context.Context, string, int64) (Pipeline, error) {
	return *f.pipeline, nil
}
func (f *fakeMR) FailedJobs(context.Context, string, int64) ([]PipelineJob, error) {
	return f.failed, nil
}
func (f *fakeMR) JobLogTail(context.Context, string, int64, int) (string, error) {
	return "error CS1002: ; expected", nil
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
	git(t, clone, "push", "origin", "HEAD:test")

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
	if mr.req == nil || mr.req.Title != "[X-1] Crash on save" || !mr.req.Draft || !strings.Contains(mr.req.Description, "✅") ||
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

type fakeComment struct {
	key, body   string
	attachments []string
}

func (f *fakeComment) AddComment(_ context.Context, key, body string) error {
	f.key, f.body = key, body
	return nil
}

func (f *fakeComment) AddAttachment(_ context.Context, _, path, name string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	f.attachments = append(f.attachments, name)
	return name, nil
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

func TestPublishAfterMRFailure(t *testing.T) {
	ctx := context.Background()
	w, tg, _, origin := setup(t, okClaude)
	mr := &fakeMR{err: errors.New("gitlab create MR: HTTP 403: insufficient_scope")}
	w.MR = mr
	jc := &fakeComment{}
	w.Comment = jc

	w.RunOnce(ctx)
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobFailed || !strings.Contains(job.Error, "insufficient_scope") {
		t.Fatalf("job = %+v", job)
	}
	// Failure after a commit still reports where the work is.
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "Failed at") || !strings.Contains(tg.sends[0], "agent/X-1-j1") {
		t.Fatalf("failure report = %v", tg.sends)
	}

	mr.err = nil // token fixed
	if err := w.Publish(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	job, _ = w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobSucceeded || job.MRURL != "https://gitlab/mr/1" || job.TestsPassed == nil || !*job.TestsPassed {
		t.Fatalf("published job = %+v", job)
	}
	if !strings.Contains(mr.req.Description, "Root cause") || !strings.Contains(mr.req.Description, "✅") {
		t.Errorf("MR description = %q", mr.req.Description)
	}
	if jc.key != "X-1" || !strings.Contains(jc.body, "https://gitlab/mr/1") {
		t.Errorf("jira comment = %+v", jc)
	}
	if out := git(t, origin, "branch", "--list", "agent/X-1-j1"); out == "" {
		t.Error("branch missing on origin")
	}
	if err := w.Publish(ctx, job.ID); err == nil {
		t.Error("second publish should refuse (MR exists)")
	}
}

// e2eClaude is a fake agent that checks the E2E contract (env, rules, deny rule, evidence dir
// outside the worktree), then fixes the bug and leaves screenshots + e2e.json.
const e2eClaude = `test "$E2E_BASE_URL" = "http://localhost:8803" || exit 11
test "$E2E_PROFILE" = "local-maker" || exit 12
test -n "$E2E_CREDENTIALS_FILE" || exit 13
test -d "$EVIDENCE_DIR" || exit 14
case "$EVIDENCE_DIR" in "$PWD"*) exit 15;; esac
case "$*" in *"E2E web verification rules"*) ;; *) exit 16;; esac
case "$*" in *"Read(/$E2E_CREDENTIALS_FILE)"*) ;; *) exit 17;; esac
case "$*" in *"Bash(node:*)"*) ;; *) exit 18;; esac
printf png > "$EVIDENCE_DIR/01-before.png"
printf png > "$EVIDENCE_DIR/02-after.png"
cat > "$EVIDENCE_DIR/e2e.json" <<'J'
{"environment":"local BO","scenarios":[
 {"id":"S1","name":"Save | trigger","expected":"no crash","result":"PASS","screenshots":["02-after.png"]}],"blocked":""}
J
` + okClaude

func TestPipelineE2EEvidence(t *testing.T) {
	ctx := context.Background()
	w, tg, _, _ := setup(t, e2eClaude)
	w.Repos["X"][0].E2E = &repos.E2E{BaseURL: "http://localhost:8803", StartCmd: "dotnet run", CredentialsProfile: "local-maker"}
	w.CredentialsFile = filepath.Join(t.TempDir(), "credentials.json")
	os.WriteFile(w.CredentialsFile, []byte(`{"profiles":{}}`), 0o600)
	mr := &fakeMR{}
	w.MR = mr
	jc := &fakeComment{}
	w.Comment = jc

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := w.Store.LatestJob(ctx, "X-1")
	if job.Status != store.JobSucceeded {
		t.Fatalf("job = %+v", job)
	}
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "E2E: ✅ 1/1 PASS · 2 screenshots") {
		t.Fatalf("telegram report = %v", tg.sends)
	}
	if !strings.Contains(mr.req.Description, "| S1 | Save \\| trigger |") || !strings.Contains(mr.req.Description, "Co-authored-by: Claude") {
		t.Errorf("MR description = %q", mr.req.Description)
	}
	want := []string{"X-1-j1-01-before.png", "X-1-j1-02-after.png"}
	if strings.Join(jc.attachments, ",") != strings.Join(want, ",") {
		t.Fatalf("attachments = %v", jc.attachments)
	}
	for _, s := range []string{"!X-1-j1-02-after.png|thumbnail!", "Other screenshots: !X-1-j1-01-before.png|thumbnail!",
		"Save ¦ trigger", "(/) PASS", "{{Co-authored-by: Claude Code <noreply@anthropic.com>}}"} {
		if !strings.Contains(jc.body, s) {
			t.Errorf("jira comment missing %q:\n%s", s, jc.body)
		}
	}
}

func TestPipelineE2EMissingEvidenceIsFlagged(t *testing.T) {
	ctx := context.Background()
	w, tg, _, _ := setup(t, okClaude) // agent fixes but never verifies in a browser
	w.Repos["X"][0].E2E = &repos.E2E{BaseURL: "http://localhost:8803", CredentialsProfile: "p"}
	w.CredentialsFile = filepath.Join(t.TempDir(), "missing.json")
	w.NoPush = true

	w.RunOnce(ctx)
	if len(tg.sends) != 1 || !strings.Contains(tg.sends[0], "E2E: ⚠️ incomplete — agent wrote no e2e.json") {
		t.Fatalf("report = %v", tg.sends)
	}
	md, _ := os.ReadFile(filepath.Join(w.JobsDir, "1", "report.md"))
	if !strings.Contains(string(md), "## E2E") {
		t.Errorf("report.md = %s", md)
	}
}

func TestEvidenceLine(t *testing.T) {
	dir := t.TempDir()
	write := func(js string) *Evidence {
		os.WriteFile(filepath.Join(dir, "e2e.json"), []byte(js), 0o644)
		return loadEvidence(dir)
	}
	if l := write(`{"scenarios":[{"id":"S1","result":"PASS"}]}`).Line(); !strings.Contains(l, "no screenshots") {
		t.Errorf("no shots: %s", l)
	}
	os.WriteFile(filepath.Join(dir, "01.png"), nil, 0o644)
	if l := write(`{"scenarios":[{"id":"S1","result":"PASS"},{"id":"S2","result":"FAIL"}]}`).Line(); !strings.HasPrefix(l, "❌ 1/2 FAIL") {
		t.Errorf("fail: %s", l)
	}
	if l := write(`{"scenarios":[],"blocked":"no credentials profile"}`).Line(); !strings.Contains(l, "BLOCKED") {
		t.Errorf("blocked: %s", l)
	}
	if e := write(`{"scenarios":[{"id":"S1","result":"PASS"}]}`); !e.Complete() || !strings.HasPrefix(e.Line(), "✅") {
		t.Errorf("complete: %s", e.Line())
	}
}
