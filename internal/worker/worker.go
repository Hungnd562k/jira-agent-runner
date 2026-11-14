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

// Commenter posts a comment (with uploaded screenshots) on a Jira issue.
type Commenter interface {
	AddComment(ctx context.Context, key, body string) error
	AddAttachment(ctx context.Context, key, path, name string) (string, error)
}

// Messenger is the Telegram surface the worker needs (no getUpdates: the notifier owns polling).
type Messenger interface {
	SendMessage(ctx context.Context, chatID int64, html string, kb telegram.Keyboard, replyTo int64) (int64, error)
	EditMessage(ctx context.Context, chatID, messageID int64, html string, kb telegram.Keyboard) error
}

// MRRequest describes a merge request to open.
type MRRequest struct {
	Repo         repos.Repo
	SourceBranch string
	Target       string // empty = Repo.BaseBranch
	Title        string
	Description  string
	Draft        bool
	KeepSource   bool // never delete the source branch (handover MRs from the shared dev branch)
}

// MR identifies an opened merge request.
type MR struct {
	IID    int64
	WebURL string
}

// Pipeline is a CI/CD pipeline's status (created, pending, running, success, failed, canceled, skipped, manual…).
type Pipeline struct {
	ID     int64
	Status string
	WebURL string
}

// PipelineJob is a failed CI/CD job.
type PipelineJob struct {
	ID                                 int64
	Name, Stage, WebURL, FailureReason string
}

// GitLab is the code-host surface the worker needs.
type GitLab interface {
	OpenMR(ctx context.Context, r MRRequest) (MR, error)
	FindOpenMR(ctx context.Context, project, source, target string) (MR, bool, error)
	ApproveMR(ctx context.Context, project string, iid int64) error
	MergeMR(ctx context.Context, project string, iid int64, removeSource bool) (string, error)
	FindPipeline(ctx context.Context, project, ref, sha string) (Pipeline, bool, error)
	GetPipeline(ctx context.Context, project string, id int64) (Pipeline, error)
	FailedJobs(ctx context.Context, project string, pipelineID int64) ([]PipelineJob, error)
	JobLogTail(ctx context.Context, project string, jobID int64, lines int) (string, error)
}

// Resolver moves a delivered Jira issue on: workflow transition + back to the reporter.
type Resolver interface {
	Transition(ctx context.Context, key, toStatus, resolution string) (string, error)
	AssignToReporter(ctx context.Context, key string) (string, error)
}

// Worker runs agent jobs one at a time.
type Worker struct {
	Store       *store.Store
	Repos       repos.Config
	Jira        IssueGetter
	TG          Messenger
	ChatID      int64
	MR          GitLab    // nil or NoPush → commit locally only
	Comment     Commenter // optional: comment on the Jira issue once an MR exists
	Resolver    Resolver  // optional: resolve + reassign issues after a successful handover
	Handover    HandoverConfig
	NoPush      bool
	Claude      agent.ClaudeSpec // Bin, MaxBudgetUSD, Model, Timeout; Dir/Prompt/Tools/LogPath set per job
	TestTimeout time.Duration
	// E2E: login profiles file and NODE_PATH handed to the agent for browser verification.
	CredentialsFile string
	NodePath        string
	WorktreeRoot    string
	JobsDir         string // per-job logs: <JobsDir>/<id>/{agent.json,test.log}
	PollEvery       time.Duration
	Now             func() time.Time
	Log             *slog.Logger
}

// Run recovers orphaned jobs, then processes the queue until ctx is done. Approved handovers
// run before the next job; a handover is proposed only when the queue is idle; CI/CD of
// deployed handovers is watched in the background.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.recover(ctx); err != nil {
		return err
	}
	every := w.PollEvery
	if every <= 0 {
		every = 10 * time.Second
	}
	handover := w.MR != nil && !w.NoPush
	if handover {
		go w.watchLoop(ctx)
	}
	for ctx.Err() == nil {
		if handover {
			w.runApprovedHandover(ctx)
		}
		did, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("worker", "err", err)
		}
		if did {
			continue
		}
		if handover {
			if err := w.proposeHandover(ctx); err != nil && ctx.Err() == nil {
				w.Log.Error("propose handover", "err", err)
			}
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
	spec.AllowedTools = repo.AllowedTools
	spec.LogPath = filepath.Join(jobDir, "agent.json")
	task := agent.Task{TestCmd: repo.TestCmd}
	if repo.E2E != nil {
		e2e, err := w.prepareE2E(log, &spec, repo, jobDir)
		if err != nil {
			return fail("prepare e2e: %v", err)
		}
		task.E2E = e2e
	}
	spec.SystemPrompt = agent.Rules(task.E2E != nil)
	spec.Prompt = agent.BuildPrompt(issue, task)
	log.Info("agent running", "worktree", wt.Dir)
	cr, err := agent.RunClaude(ctx, spec)
	if err != nil {
		return fail("%v", err)
	}
	log.Info("agent done", "cost_usd", cr.CostUSD, "turns", cr.NumTurns)
	rep.Agent, rep.Branch = cr, branch
	if task.E2E != nil {
		rep.Evidence = loadEvidence(task.E2E.EvidenceDir)
		log.Info("e2e evidence", "status", rep.Evidence.Line())
	}

	changed, err := wt.HasChanges(ctx)
	if err != nil {
		return fail("git status: %v", err)
	}
	if !changed {
		detail = cr.Text
		return fail("agent made no changes")
	}

	if repo.TestCmd != "" {
		tr, err := agent.RunTests(ctx, wt.Dir, repo.TestCmd, filepath.Join(jobDir, "test.log"), w.TestTimeout)
		if err != nil {
			return fail("%v", err)
		}
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
	if err := w.openAndMerge(ctx, log, issue, repo, branch, rep, &res); err != nil {
		return fail("%v", err)
	}
	return res, ""
}

// mergeGate decides whether a fix may go straight into the shared base branch:
// the repo opted in, tests ran and passed, and E2E (when configured) is complete.
func mergeGate(repo repos.Repo, tests *agent.TestRun, ev *Evidence) (bool, string) {
	switch {
	case !repo.AutoMerge:
		return false, "auto_merge is off for this repo"
	case tests == nil:
		return false, "no test_cmd: tests not run"
	case !tests.Passed:
		return false, "tests failed"
	case repo.E2E != nil && (ev == nil || !ev.Complete()):
		return false, "E2E not complete"
	}
	return true, ""
}

// openAndMerge opens the MR for a pushed branch: a Draft MR when the gate fails, otherwise
// a ready MR that is approved and merged into base_branch (the job then waits for handover).
// A failed merge leaves the MR open and is reported, not treated as a job failure.
func (w *Worker) openAndMerge(ctx context.Context, log *slog.Logger, issue jira.IssueDetail, repo repos.Repo, branch string, rep *Report, res *store.JobResult) error {
	ok, why := mergeGate(repo, rep.Tests, rep.Evidence)
	rep.MergeNote = why
	mr, err := w.MR.OpenMR(ctx, MRRequest{
		Repo:         repo,
		SourceBranch: branch,
		Title:        fmt.Sprintf("[%s] %s", issue.Key, issue.Summary),
		Description:  mrDescription(issue, repo, rep.Agent, rep.Tests, rep.Evidence, rep.DiffStat, ok),
		Draft:        !ok,
	})
	if err != nil {
		return fmt.Errorf("open MR: %w", err)
	}
	res.MRURL, res.MRIID = mr.WebURL, mr.IID
	rep.MRURL = mr.WebURL
	if !ok {
		return nil
	}
	if err := w.MR.ApproveMR(ctx, repo.GitLabProject, mr.IID); err != nil {
		log.Warn("approve MR (merging anyway)", "err", err)
	}
	sha, err := w.MR.MergeMR(ctx, repo.GitLabProject, mr.IID, true)
	if err != nil {
		log.Warn("auto-merge failed; MR left open", "err", err)
		rep.MergeNote = "merge failed: " + err.Error()
		return nil
	}
	res.MergedSHA = sha
	rep.Merged = true
	log.Info("merged into base branch", "branch", repo.BaseBranch, "sha", sha)
	return nil
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

func mrDescription(d jira.IssueDetail, repo repos.Repo, cr agent.ClaudeResult, tests *agent.TestRun, ev *Evidence, stat string, autoMerge bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Jira: [%s](%s) — %s\n\n", d.Key, d.URL, d.Summary)
	if autoMerge {
		fmt.Fprintf(&b, "> 🤖 Generated by jira-agent-runner with Claude Code. Tests and E2E passed: auto-merged into `%s`; handed over after confirmation on Telegram.\n\n", repo.BaseBranch)
	} else {
		b.WriteString("> 🤖 Generated by jira-agent-runner with Claude Code. Review carefully before marking ready.\n\n")
	}
	switch {
	case tests == nil:
		b.WriteString("**Tests:** ⚠️ no `test_cmd` configured — not run\n\n")
	case tests.Passed:
		fmt.Fprintf(&b, "**Tests:** ✅ `%s` passed (%s)\n\n", repo.TestCmd, tests.Duration.Round(time.Second))
	default:
		fmt.Fprintf(&b, "**Tests:** ❌ `%s` failed with exit %d — **do not merge as is**\n\n", repo.TestCmd, tests.ExitCode)
	}
	if ev != nil {
		b.WriteString("## E2E\n\n" + ev.Markdown())
		fmt.Fprintf(&b, "Screenshots are attached to [%s](%s).\n\n", d.Key, d.URL)
	}
	b.WriteString("## Agent report\n\n" + strings.TrimSpace(cr.Text) + "\n\n")
	if stat != "" {
		b.WriteString("## Diff stat\n\n```\n" + stat + "\n```\n\n")
	}
	if tests != nil {
		b.WriteString("<details><summary>Test output (tail)</summary>\n\n```\n" + tests.Tail + "\n```\n</details>\n\n")
	}
	fmt.Fprintf(&b, "_Agent cost: $%.2f, %d turns_\n\n%s\n", cr.CostUSD, cr.NumTurns, coAuthor)
	return b.String()
}

// coAuthor ends every MR description and Jira comment the agent writes.
const coAuthor = "Co-authored-by: Claude Code <noreply@anthropic.com>"

// e2eTools let the agent run its Playwright scripts, probe and restart the local app.
// The app's own start/build commands come from the repo's allowed_tools.
var e2eTools = []string{"Bash(node:*)", "Bash(npx playwright:*)", "Bash(curl:*)", "Bash(sleep:*)", "Bash(kill:*)"}

// prepareE2E creates the evidence dir (outside the worktree, never committed) and hands the
// agent the app, the credentials profile and the file path — never the credential values.
// The agent's file tools are denied on the credentials file; only its scripts read it.
func (w *Worker) prepareE2E(log *slog.Logger, spec *agent.ClaudeSpec, repo repos.Repo, jobDir string) (*agent.E2E, error) {
	evDir, err := filepath.Abs(filepath.Join(jobDir, "evidence"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		return nil, err
	}
	cred := w.CredentialsFile
	if fi, err := os.Stat(cred); err != nil {
		log.Warn("e2e credentials file missing: the agent will report E2E as blocked", "path", cred)
	} else if fi.Mode().Perm()&0o077 != 0 {
		log.Warn("e2e credentials file is readable by others; chmod 600", "path", cred)
	}
	e := &agent.E2E{BaseURL: repo.E2E.BaseURL, StartCmd: repo.E2E.StartCmd, Profile: repo.E2E.CredentialsProfile,
		Notes: repo.E2E.Notes, EvidenceDir: evDir}
	spec.AllowedTools = append(append([]string{}, repo.AllowedTools...), e2eTools...)
	spec.AddDirs = append(spec.AddDirs, evDir)
	// "//" marks an absolute path in Claude Code permission rules.
	spec.Disallowed = append(spec.Disallowed, "Read(/"+cred+")", "Edit(/"+cred+")", "Write(/"+cred+")")
	spec.Env = append(spec.Env, "E2E_BASE_URL="+e.BaseURL, "E2E_START_CMD="+e.StartCmd, "E2E_PROFILE="+e.Profile,
		"E2E_CREDENTIALS_FILE="+cred, "EVIDENCE_DIR="+evDir)
	if w.NodePath != "" {
		spec.Env = append(spec.Env, "NODE_PATH="+w.NodePath)
	}
	return e, nil
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
		w.uploadEvidence(ctx, log, j, rep.Evidence)
		if err := w.Comment.AddComment(ctx, j.JiraKey, rep.JiraComment()); err != nil {
			log.Warn("jira comment", "err", err)
		}
	}
}

// uploadEvidence attaches the job's screenshots to the issue as <KEY>-j<id>-<shot> so retries
// never overwrite earlier evidence. Failed uploads are shown as "not uploaded" in the comment.
func (w *Worker) uploadEvidence(ctx context.Context, log *slog.Logger, j store.Job, ev *Evidence) {
	if ev == nil || len(ev.Shots) == 0 {
		return
	}
	ev.Uploaded = map[string]string{}
	for _, shot := range ev.Shots {
		name := fmt.Sprintf("%s-j%d-%s", j.JiraKey, j.ID, shot)
		got, err := w.Comment.AddAttachment(ctx, j.JiraKey, filepath.Join(ev.Dir, shot), name)
		if err != nil {
			log.Warn("jira attachment", "file", shot, "err", err)
			continue
		}
		ev.Uploaded[shot] = got
	}
}
