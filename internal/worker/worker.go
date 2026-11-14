// Package worker claims queued agent jobs and runs the fix pipeline on the host:
// worktree → Claude Code → tests → commit → (push + draft MR) → Telegram update.
package worker

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/scheduler"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"
)

// IssueGetter fetches issue details for the prompt.
type IssueGetter interface {
	GetIssue(ctx context.Context, key string) (jira.IssueDetail, error)
}

// Commenter posts a comment on a Jira issue.
type Commenter interface {
	AddComment(ctx context.Context, key, body string) error
}

// Messenger is the Telegram surface the worker needs (no getUpdates: the notifier owns polling).
type Messenger interface {
	SendMessage(ctx context.Context, chatID int64, html string, kb telegram.Keyboard, replyTo int64) (int64, error)
	EditMessage(ctx context.Context, chatID, messageID int64, html string, kb telegram.Keyboard) error
}

// MRRequest describes the merge request to open for a finished job.
type MRRequest struct {
	Repo         repos.Repo
	SourceBranch string
	Title        string
	Description  string
}

// MROpener opens a draft merge request and returns its web URL.
type MROpener interface {
	OpenDraftMR(ctx context.Context, r MRRequest) (string, error)
}

// Worker runs agent jobs one at a time.
type Worker struct {
	Store        *store.Store
	Repos        repos.Config
	Jira         IssueGetter
	TG           Messenger
	ChatID       int64
	MR           MROpener  // nil or NoPush → commit locally only
	Comment      Commenter // optional: comment on the Jira issue once a Draft MR exists
	NoPush       bool
	Claude       agent.ClaudeSpec // Bin, MaxBudgetUSD, Model, Timeout; Dir/Prompt/Tools/LogPath set per job
	TestTimeout  time.Duration
	WorktreeRoot string
	JobsDir      string // per-job logs: <JobsDir>/<id>/{agent.json,test.log}
	PollEvery    time.Duration
	Now          func() time.Time
	Log          *slog.Logger
}

// Run recovers orphaned jobs, then processes the queue until ctx is done.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.recover(ctx); err != nil {
		return err
	}
	every := w.PollEvery
	if every <= 0 {
		every = 10 * time.Second
	}
	for ctx.Err() == nil {
		did, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("worker", "err", err)
		}
		if did {
			continue
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	return nil
}

func (w *Worker) recover(ctx context.Context) error {
	orphans, err := w.Store.FailOrphaned(ctx, w.Now())
	if err != nil {
		return err
	}
	for _, j := range orphans {
		w.Log.Warn("orphaned job marked failed", "job", j.ID, "key", j.JiraKey)
		w.updateNotice(ctx, j)
	}
	return nil
}

// RunOnce claims and processes one job. did=false when the queue was empty.
func (w *Worker) RunOnce(ctx context.Context) (did bool, err error) {
	job, ok, err := w.Store.ClaimJob(ctx, w.Now())
	if err != nil || !ok {
		return false, err
	}
	log := w.Log.With("job", job.ID, "key", job.JiraKey, "repo", job.RepoName)
	log.Info("job started")
	w.updateNotice(ctx, job)

	rep := &Report{}
	res, detail := w.process(ctx, log, job, rep)
	if res.Status == "" {
		res.Status = store.JobSucceeded
	}
	// Persist the outcome even if ctx was cancelled mid-run.
	done, ferr := w.Store.FinishJob(context.WithoutCancel(ctx), job.ID, res, w.Now())
	if ferr != nil {
		return true, ferr
	}
	log.Info("job finished", "status", done.Status, "mr", done.MRURL, "err", done.Error)
	w.updateNotice(context.WithoutCancel(ctx), done)
	if done.Status == store.JobFailed && detail != "" {
		w.reply(context.WithoutCancel(ctx), done.JiraKey, "<pre>"+html.EscapeString(truncate(detail, 3000))+"</pre>")
	}
	w.publish(context.WithoutCancel(ctx), log, done, rep)
	return true, nil
}

// process runs the pipeline; detail is extra log text shown on failure.
func (w *Worker) process(ctx context.Context, log *slog.Logger, job store.Job, rep *Report) (res store.JobResult, detail string) {
	fail := func(format string, a ...any) (store.JobResult, string) {
		return store.JobResult{Status: store.JobFailed, Error: fmt.Sprintf(format, a...)}, detail
	}

	repo, ok := w.Repos.Find(job.JiraKey, job.RepoName)
	if !ok {
		return fail("repo %q not found in repos.json", job.RepoName)
	}
	issue, err := w.Jira.GetIssue(ctx, job.JiraKey)
	if err != nil {
		return fail("load issue: %v", err)
	}
	rep.Key, rep.Summary, rep.IssueURL, rep.Repo = issue.Key, issue.Summary, issue.URL, repo

	jobDir := filepath.Join(w.JobsDir, strconv.FormatInt(job.ID, 10))
	rep.JobDir = jobDir
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return fail("create job dir: %v", err)
	}

	branch := fmt.Sprintf("agent/%s-j%d", job.JiraKey, job.ID)
	root, name := worktreeLocation(w.WorktreeRoot, repo.Path, job)
	wt, err := agent.NewWorktree(ctx, repo.Path, root, name, branch, repo.BaseBranch)
	if err != nil {
		return fail("prepare worktree: %v", err)
	}
	// Git-ignored agent guidance (CLAUDE.md/AGENTS.md) only exists in the main clone.
	agent.CopyIgnoredFiles(repo.Path, wt.Dir, "CLAUDE.md", "AGENTS.md", "CLAUDE.local.md")
	defer func() {
		if err := wt.Remove(context.WithoutCancel(ctx)); err != nil {
			log.Warn("remove worktree", "err", err)
		}
	}()
	if err := w.Store.SetJobBranch(ctx, job.ID, branch); err != nil {
		return fail("save branch: %v", err)
	}

	spec := w.Claude
	spec.Dir = wt.Dir
	spec.Prompt = agent.BuildPrompt(issue, repo.TestCmd)
	spec.AllowedTools = repo.AllowedTools
	spec.LogPath = filepath.Join(jobDir, "agent.json")
	log.Info("agent running", "worktree", wt.Dir)
	cr, err := agent.RunClaude(ctx, spec)
	if err != nil {
		return fail("%v", err)
	}
	log.Info("agent done", "cost_usd", cr.CostUSD, "turns", cr.NumTurns)
	rep.Agent, rep.Branch = cr, branch

	changed, err := wt.HasChanges(ctx)
	if err != nil {
		return fail("git status: %v", err)
	}
	if !changed {
		detail = cr.Text
		return fail("agent made no changes")
	}

	var tests *agent.TestRun
	if repo.TestCmd != "" {
		tr, err := agent.RunTests(ctx, wt.Dir, repo.TestCmd, filepath.Join(jobDir, "test.log"), w.TestTimeout)
		if err != nil {
			return fail("%v", err)
		}
		tests = &tr
		rep.Tests = &tr
		res.TestsPassed = &tr.Passed
		log.Info("tests done", "passed", tr.Passed, "exit", tr.ExitCode)
	}

	stat := wt.DiffStat(ctx)
	rep.DiffStat = stat
	if err := wt.Commit(ctx, commitMessage(issue)); err != nil {
		return fail("commit: %v", err)
	}
	if w.NoPush || w.MR == nil {
		return res, ""
	}

	if err := wt.Push(ctx); err != nil {
		return fail("push: %v", err)
	}
	url, err := w.MR.OpenDraftMR(ctx, MRRequest{
		Repo:         repo,
		SourceBranch: branch,
		Title:        fmt.Sprintf("Draft: [%s] %s", issue.Key, issue.Summary),
		Description:  mrDescription(issue, repo, cr, tests, stat),
	})
	if err != nil {
		return fail("open MR: %v", err)
	}
	res.MRURL = url
	rep.MRURL = url
	return res, ""
}

// updateNotice re-renders the task's latest notice with the job's progress.
func (w *Worker) updateNotice(ctx context.Context, j store.Job) {
	t, err := w.Store.Get(ctx, j.JiraKey)
	if err != nil || t.TGMessageID == 0 {
		return
	}
	if err := w.TG.EditMessage(ctx, w.ChatID, t.TGMessageID, scheduler.ApprovedText(t, &j), scheduler.JobKeyboard(t, &j)); err != nil && !telegram.IsNotModified(err) {
		w.Log.Warn("edit notice", "key", j.JiraKey, "err", err)
	}
}

func (w *Worker) reply(ctx context.Context, key, text string) {
	t, err := w.Store.Get(ctx, key)
	if err != nil {
		return
	}
	if _, err := w.TG.SendMessage(ctx, w.ChatID, text, nil, t.TGMessageID); err != nil {
		w.Log.Warn("send detail", "key", key, "err", err)
	}
}

func commitMessage(d jira.IssueDetail) string {
	return fmt.Sprintf("fix(%s): %s\n\nJira: %s\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n", d.Key, d.Summary, d.URL)
}

func mrDescription(d jira.IssueDetail, repo repos.Repo, cr agent.ClaudeResult, tests *agent.TestRun, stat string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Jira: [%s](%s) — %s\n\n", d.Key, d.URL, d.Summary)
	b.WriteString("> 🤖 Generated by jira-agent-runner with Claude Code. Review carefully before marking ready.\n\n")
	switch {
	case tests == nil:
		b.WriteString("**Tests:** ⚠️ no `test_cmd` configured — not run\n\n")
	case tests.Passed:
		fmt.Fprintf(&b, "**Tests:** ✅ `%s` passed (%s)\n\n", repo.TestCmd, tests.Duration.Round(time.Second))
	default:
		fmt.Fprintf(&b, "**Tests:** ❌ `%s` failed with exit %d — **do not merge as is**\n\n", repo.TestCmd, tests.ExitCode)
	}
	b.WriteString("## Agent report\n\n" + strings.TrimSpace(cr.Text) + "\n\n")
	if stat != "" {
		b.WriteString("## Diff stat\n\n```\n" + stat + "\n```\n\n")
	}
	if tests != nil {
		b.WriteString("<details><summary>Test output (tail)</summary>\n\n```\n" + tests.Tail + "\n```\n</details>\n\n")
	}
	fmt.Fprintf(&b, "_Agent cost: $%.2f, %d turns_\n", cr.CostUSD, cr.NumTurns)
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// worktreeLocation picks where a job's worktree lives. With no explicit root it sits next to
// the main clone (same directory depth), so relative references to sibling repos such as
// ../../other-repo/x.csproj resolve exactly as they do in the main clone.
func worktreeLocation(root, repoPath string, job store.Job) (dir, name string) {
	name = fmt.Sprintf("%s-j%d", job.JiraKey, job.ID)
	if root != "" {
		return root, name
	}
	return filepath.Dir(repoPath), ".agent-" + filepath.Base(repoPath) + "-" + name
}

// publish shares a finished job's report: report.md on disk, a Telegram reply, and a Jira
// comment when a Draft MR was opened. Failures here never change the job's outcome.
func (w *Worker) publish(ctx context.Context, log *slog.Logger, j store.Job, rep *Report) {
	if !rep.ready() {
		return
	}
	if err := os.WriteFile(filepath.Join(rep.JobDir, "report.md"), []byte(rep.Markdown(j)), 0o644); err != nil {
		log.Warn("write report", "err", err)
	}
	if j.Status == store.JobFailed && rep.DiffStat == "" {
		return // nothing was committed; the failure reply already explains it
	}
	text := rep.Telegram()
	if j.Status == store.JobFailed {
		// The agent ran (and maybe pushed): say where the work is and which step failed.
		text = "⚠️ <b>Failed at:</b> " + html.EscapeString(truncate(j.Error, 300)) + "\n" + text
	}
	w.reply(ctx, j.JiraKey, text)
	if rep.MRURL != "" && w.Comment != nil {
		if err := w.Comment.AddComment(ctx, j.JiraKey, rep.JiraComment()); err != nil {
			log.Warn("jira comment", "err", err)
		}
	}
}
