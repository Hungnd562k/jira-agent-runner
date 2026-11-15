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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/scheduler"
	"github.com/Hungnd562k/jira-agent-runner/internal/scope"
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
	Scope       scope.Format // maintainer's scope comment: specs/hints go into the prompt
	NoPush      bool
	Claude      agent.ClaudeSpec // Bin, MaxBudgetUSD, Model, Timeout; Dir/Prompt/Tools/LogPath set per job
	TestTimeout time.Duration
	// E2E: login profiles file and NODE_PATH handed to the agent for browser verification.
	CredentialsFile string
	NodePath        string
	WorktreeRoot    string
	JobsDir         string // per-job logs: <JobsDir>/<id>/{agent.json,test.log}
	PollEvery       time.Duration
	// Resilience: automatic retries of transient failures (network/API), with exponential
	// backoff from RetryDelay; git fetch/push are bounded by GitNetTimeout.
	AutoRetries   int
	RetryDelay    time.Duration
	GitNetTimeout time.Duration
	BootID        string // agent.BootID(); lets a restarted worker reap an orphaned agent safely
	Now           func() time.Time
	Log           *slog.Logger
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

// RunOnce claims and processes one job group (one job per repo of the scope, one agent run).
// did=false when the queue was empty.
func (w *Worker) RunOnce(ctx context.Context) (did bool, err error) {
	jobs, ok, err := w.Store.ClaimGroup(ctx, w.Now())
	if err != nil || !ok {
		return false, err
	}
	key := jobs[0].JiraKey
	log := w.Log.With("group", jobs[0].GroupID, "key", key, "repos", repoNames(jobs))
	log.Info("job started", "jobs", len(jobs))
	w.updateNotice(ctx, key)

	runs := w.process(ctx, log, jobs)
	stopped := ctx.Err() != nil
	// Persist the outcomes even if ctx was cancelled mid-run.
	bg := context.WithoutCancel(ctx)
	done := make([]store.Job, len(runs))
	for i, r := range runs {
		if r.res.Status == "" {
			r.res.Status = store.JobSucceeded
		}
		if stopped && r.res.Status == store.JobFailed {
			r.res.Error = "interrupted (worker stopped): " + r.res.Error
		}
		d, ferr := w.Store.FinishJob(bg, r.job.ID, r.res, w.Now())
		if ferr != nil {
			return true, ferr
		}
		done[i] = d
		log.Info("job finished", "job", d.ID, "repo", d.RepoName, "status", d.Status, "mr", d.MRURL, "err", d.Error)
	}
	w.updateNotice(bg, key)
	retried := w.maybeRetry(bg, log, done, stopped)
	shown := map[string]bool{}
	for i, d := range done {
		if retried[d.ID] {
			continue
		}
		if det := runs[i].detail; d.Status == store.JobFailed && det != "" && !shown[det] {
			shown[det] = true // one agent run: show its output once, not once per repo
			w.reply(bg, key, "<pre>"+html.EscapeString(truncate(det, 3000))+"</pre>")
		}
		if d.WIPSHA != "" {
			w.reply(bg, key, wipText(d))
		}
		w.publish(bg, log, d, runs[i].rep)
	}
	return true, nil
}

// jobRun is one job (= one repo) of the group being processed.
type jobRun struct {
	job     store.Job
	repo    repos.Repo
	wt      *agent.Worktree
	jobDir  string
	branch  string
	e2e     *agent.E2E
	changed bool
	res     store.JobResult
	detail  string // extra log text shown on failure
	rep     *Report
}

func (r *jobRun) failed() bool { return r.res.Status == store.JobFailed }

func (r *jobRun) fail(format string, a ...any) {
	r.res = store.JobResult{Status: store.JobFailed, Error: fmt.Sprintf(format, a...)}
}

// process runs the pipeline for a group: worktrees for every repo, one agent session over all
// of them, then tests → commit → push → MR per repo that changed.
func (w *Worker) process(ctx context.Context, log *slog.Logger, jobs []store.Job) []*jobRun {
	runs := make([]*jobRun, len(jobs))
	for i, j := range jobs {
		runs[i] = &jobRun{job: j, rep: &Report{}}
	}
	failAll := func(format string, a ...any) []*jobRun {
		for _, r := range runs {
			if !r.failed() {
				r.fail(format, a...)
			}
		}
		return runs
	}

	for _, r := range runs {
		repo, ok := w.Repos.Find(r.job.JiraKey, r.job.RepoName)
		if !ok {
			return failAll("repo %q not found in repos.json", r.job.RepoName)
		}
		r.repo = repo
	}
	key := jobs[0].JiraKey
	issue, err := w.Jira.GetIssue(ctx, key)
	if err != nil {
		return failAll("load issue: %v", err)
	}
	for _, r := range runs {
		r.rep.Key, r.rep.Summary, r.rep.IssueURL, r.rep.Repo = issue.Key, issue.Summary, issue.URL, r.repo
	}

	defer func() {
		bg := context.WithoutCancel(ctx)
		for _, r := range runs {
			if r.wt == nil {
				continue
			}
			// A failed run keeps the agent's uncommitted edits as a local WIP commit, so a retry
			// (automatic or manual) continues from them instead of starting over.
			if r.failed() {
				if sha, err := r.wt.SaveWIP(bg, wipMessage(r.job, r.res.Error)); err != nil {
					log.Warn("save wip", "repo", r.repo.Name, "err", err)
				} else if sha != "" {
					r.res.WIPSHA = sha
					log.Info("partial work saved", "repo", r.repo.Name, "sha", shortSHA(sha))
				}
			}
			if err := r.wt.Remove(bg); err != nil {
				log.Warn("remove worktree", "repo", r.repo.Name, "err", err)
			}
		}
	}()

	spec := w.Claude
	task := agent.Task{}
	if sc := w.scopeFor(issue); sc != nil {
		task.ScopeAuthor = sc.Author
		for _, n := range sc.Notes {
			task.Scope = append(task.Scope, agent.ScopeNote{Key: n.Key, Description: n.Description, Value: n.Value})
		}
	} else {
		log.Warn("no valid scope comment on the issue any more; running on the queued repos without specs/hints")
	}
	for i, r := range runs {
		r.jobDir = filepath.Join(w.JobsDir, strconv.FormatInt(r.job.ID, 10))
		r.rep.JobDir = r.jobDir
		if err := os.MkdirAll(r.jobDir, 0o755); err != nil {
			return failAll("create job dir: %v", err)
		}
		r.branch = fmt.Sprintf("agent/%s-j%d", r.job.JiraKey, r.job.ID)
		root, name := worktreeLocation(w.WorktreeRoot, r.repo.Path, r.job)
		nctx, cancel := w.netCtx(ctx)
		wt, err := agent.NewWorktree(nctx, r.repo.Path, root, name, r.branch, r.repo.BaseBranch)
		err = netErr(nctx, err)
		cancel()
		if err != nil {
			return failAll("prepare worktree %s: %v", r.repo.Name, err)
		}
		r.wt = wt
		// Git-ignored agent guidance (CLAUDE.md/AGENTS.md) only exists in the main clone.
		agent.CopyIgnoredFiles(r.repo.Path, wt.Dir, "CLAUDE.md", "AGENTS.md", "CLAUDE.local.md")
		if err := w.Store.SetJobBranch(ctx, r.job.ID, r.branch); err != nil {
			return failAll("save branch: %v", err)
		}
		rt := agent.RepoTask{Name: r.repo.Name, Dir: wt.Dir, TestCmd: r.repo.TestCmd,
			ResumeOf: w.resume(ctx, log, wt, r.job, r.repo.BaseBranch)}
		if r.repo.E2E != nil {
			e2e, err := w.prepareE2E(log, &spec, r.repo, r.jobDir, !task.HasE2E())
			if err != nil {
				return failAll("prepare e2e: %v", err)
			}
			r.e2e, rt.E2E = e2e, e2e
		}
		spec.AllowedTools = appendNew(spec.AllowedTools, r.repo.AllowedTools...)
		if i == 0 {
			spec.Dir = wt.Dir
		} else {
			spec.AddDirs = append(spec.AddDirs, wt.Dir)
		}
		task.Repos = append(task.Repos, rt)
	}

	spec.LogPath = filepath.Join(runs[0].jobDir, "agent.json")
	spec.SystemPrompt = agent.Rules(task.HasE2E())
	spec.Prompt = agent.BuildPrompt(issue, task)
	spec.OnStart = func(pgid int) {
		for _, r := range runs {
			if err := w.Store.SetJobAgent(context.WithoutCancel(ctx), r.job.ID, pgid, w.BootID); err != nil {
				log.Warn("record agent pgid", "err", err)
			}
		}
	}
	log.Info("agent running", "dir", spec.Dir, "extra_dirs", len(spec.AddDirs))
	cr, err := agent.RunClaude(ctx, spec)
	// Every job dir gets the agent log, so --publish-job works per repo.
	for _, r := range runs[1:] {
		copyFile(spec.LogPath, filepath.Join(r.jobDir, "agent.json"))
	}
	if err != nil {
		return failAll("%v", err)
	}
	log.Info("agent done", "cost_usd", cr.CostUSD, "turns", cr.NumTurns)

	anyChange := false
	for _, r := range runs {
		r.rep.Agent, r.rep.Branch = cr, r.branch
		if r.e2e != nil {
			r.rep.Evidence = loadEvidence(r.e2e.EvidenceDir)
			log.Info("e2e evidence", "repo", r.repo.Name, "status", r.rep.Evidence.Line())
		}
		changed, err := r.wt.HasChanges(ctx)
		if err != nil {
			r.fail("git status: %v", err)
			continue
		}
		r.changed = changed
		anyChange = anyChange || changed
	}
	for _, r := range runs {
		switch {
		case r.failed():
		case !r.changed && !anyChange:
			r.fail("agent made no changes")
			r.detail = cr.Text
		case !r.changed:
			// Another repo of the scope carried the fix: nothing to commit here.
			r.res = store.JobResult{Status: store.JobSucceeded, Error: "no changes needed in this repo"}
			r.rep.Branch = "" // nothing to report or publish for this repo
		default:
			w.deliverRepo(ctx, log, issue, r)
		}
	}
	return runs
}

// deliverRepo runs tests, commits and (unless NoPush) pushes and opens/merges the MR of one
// repo the agent changed. Failures only affect this repo's job.
func (w *Worker) deliverRepo(ctx context.Context, log *slog.Logger, issue jira.IssueDetail, r *jobRun) {
	log = log.With("job", r.job.ID, "repo", r.repo.Name)
	if r.repo.TestCmd != "" {
		tr, err := agent.RunTests(ctx, r.wt.Dir, r.repo.TestCmd, filepath.Join(r.jobDir, "test.log"), w.TestTimeout)
		if err != nil {
			r.fail("%v", err)
			return
		}
		r.rep.Tests = &tr
		r.res.TestsPassed = &tr.Passed
		log.Info("tests done", "passed", tr.Passed, "exit", tr.ExitCode)
	}
	r.rep.DiffStat = r.wt.DiffStat(ctx)
	if err := r.wt.Commit(ctx, commitMessage(issue)); err != nil {
		r.fail("commit: %v", err)
		return
	}
	if w.NoPush || w.MR == nil {
		return
	}
	pctx, cancel := w.netCtx(ctx)
	err := netErr(pctx, r.wt.Push(pctx))
	cancel()
	if err != nil {
		r.fail("push: %v", err)
		return
	}
	if err := w.openAndMerge(ctx, log, issue, r.repo, r.branch, r.rep, &r.res); err != nil {
		r.fail("%v", err)
	}
}

// scopeFor re-reads the maintainer's scope comment for the prompt (specs, hints).
func (w *Worker) scopeFor(issue jira.IssueDetail) *scope.Scope {
	var names []string
	for _, r := range w.Repos.For(issue.Key) {
		names = append(names, r.Name)
	}
	return w.Scope.Find(issue.Comments, names).Scope
}

func repoNames(jobs []store.Job) string {
	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.RepoName
	}
	return strings.Join(names, ",")
}

// appendNew appends the items of add that are not in list yet.
func appendNew(list []string, add ...string) []string {
	for _, a := range add {
		if !slices.Contains(list, a) {
			list = append(list, a)
		}
	}
	return list
}

func copyFile(from, to string) {
	if b, err := os.ReadFile(from); err == nil {
		_ = os.WriteFile(to, b, 0o644)
	}
}

// resume applies the WIP of the job this one continues; it returns that job's id, or 0 when
// there is nothing (or nothing applicable) to continue — the agent then starts fresh.
func (w *Worker) resume(ctx context.Context, log *slog.Logger, wt *agent.Worktree, job store.Job, base string) int64 {
	if job.ResumeOf == 0 {
		return 0
	}
	prev, err := w.Store.GetJob(ctx, job.ResumeOf)
	if err != nil || prev.WIPSHA == "" {
		return 0
	}
	if err := wt.ApplyWIP(ctx, prev.WIPSHA); err != nil {
		log.Warn("apply wip; starting fresh", "from_job", prev.ID, "err", err)
		_, _ = wt.Git().Run(ctx, "reset", "--hard", "--quiet", "origin/"+base)
		return 0
	}
	log.Info("continuing partial work", "from_job", prev.ID, "sha", shortSHA(prev.WIPSHA))
	return prev.ID
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

// updateNotice re-renders the task's latest notice with its latest job group's progress.
func (w *Worker) updateNotice(ctx context.Context, key string) {
	t, err := w.Store.Get(ctx, key)
	if err != nil || t.TGMessageID == 0 {
		return
	}
	jobs, err := w.Store.LatestGroup(ctx, key)
	if err != nil {
		return
	}
	if err := w.TG.EditMessage(ctx, w.ChatID, t.TGMessageID, scheduler.ApprovedText(t, jobs), scheduler.JobKeyboard(t, jobs)); err != nil && !telegram.IsNotModified(err) {
		w.Log.Warn("edit notice", "key", key, "err", err)
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
// first = the first E2E repo of the group: the E2E_* env vars describe it.
func (w *Worker) prepareE2E(log *slog.Logger, spec *agent.ClaudeSpec, repo repos.Repo, jobDir string, first bool) (*agent.E2E, error) {
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
	spec.AllowedTools = appendNew(spec.AllowedTools, e2eTools...)
	spec.AddDirs = append(spec.AddDirs, evDir)
	if !first {
		return e, nil
	}
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
