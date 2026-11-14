package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

// Publish recovers a job whose agent run and commit succeeded but push/MR failed
// (e.g. GitLab token without `api` scope): it pushes the existing branch, opens the Draft MR,
// marks the job succeeded and publishes the report. The agent is not re-run.
func (w *Worker) Publish(ctx context.Context, jobID int64) error {
	if w.MR == nil {
		return fmt.Errorf("publish needs GitLab settings (run without --no-push)")
	}
	job, err := w.Store.GetJob(ctx, jobID)
	if err != nil {
		return fmt.Errorf("job %d: %w", jobID, err)
	}
	if job.Status == store.JobQueued || job.Status == store.JobRunning {
		return fmt.Errorf("job %d is %s", jobID, job.Status)
	}
	if job.MRURL != "" {
		return fmt.Errorf("job %d already has an MR: %s", jobID, job.MRURL)
	}
	if job.Branch == "" {
		return fmt.Errorf("job %d has no branch (the agent never ran)", jobID)
	}
	repo, ok := w.Repos.Find(job.JiraKey, job.RepoName)
	if !ok {
		return fmt.Errorf("repo %q not found in repos.json", job.RepoName)
	}
	issue, err := w.Jira.GetIssue(ctx, job.JiraKey)
	if err != nil {
		return fmt.Errorf("load issue: %w", err)
	}

	jobDir := filepath.Join(w.JobsDir, strconv.FormatInt(job.ID, 10))
	rep := &Report{Key: issue.Key, Summary: issue.Summary, IssueURL: issue.URL, Repo: repo, Branch: job.Branch, JobDir: jobDir}
	if rep.Agent, err = loadAgentResult(filepath.Join(jobDir, "agent.json")); err != nil {
		return fmt.Errorf("agent result: %w", err)
	}
	if repo.TestCmd != "" {
		if tr, err := loadTestRun(filepath.Join(jobDir, "test.log")); err == nil {
			rep.Tests = &tr
		}
	}

	if repo.E2E != nil {
		rep.Evidence = loadEvidence(filepath.Join(jobDir, "evidence"))
	}

	g := agent.Git{Dir: repo.Path}
	if _, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+job.Branch); err != nil {
		return fmt.Errorf("local branch %s not found in %s", job.Branch, repo.Path)
	}
	rep.DiffStat, _ = g.Run(ctx, "diff", "--stat", "origin/"+repo.BaseBranch+"..."+job.Branch)
	if _, err := g.Run(ctx, "push", "origin", job.Branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	url, err := w.MR.OpenDraftMR(ctx, MRRequest{
		Repo:         repo,
		SourceBranch: job.Branch,
		Title:        fmt.Sprintf("Draft: [%s] %s", issue.Key, issue.Summary),
		Description:  mrDescription(issue, repo, rep.Agent, rep.Tests, rep.Evidence, rep.DiffStat),
	})
	if err != nil {
		return fmt.Errorf("open MR: %w", err)
	}
	rep.MRURL = url

	res := store.JobResult{Status: store.JobSucceeded, MRURL: url}
	if rep.Tests != nil {
		res.TestsPassed = &rep.Tests.Passed
	}
	done, err := w.Store.FinishJob(ctx, job.ID, res, w.Now())
	if err != nil {
		return err
	}
	log := w.Log.With("job", job.ID, "key", job.JiraKey, "repo", job.RepoName)
	log.Info("job published", "mr", url)
	w.updateNotice(ctx, done)
	w.publish(ctx, log, done, rep)
	return nil
}

// loadAgentResult parses the last JSON object of a saved `claude -p --output-format json` run.
func loadAgentResult(path string) (agent.ClaudeResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return agent.ClaudeResult{}, err
	}
	out, _, _ := bytes.Cut(b, []byte("\n--- stderr ---\n"))
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var r agent.ClaudeResult
		if json.Unmarshal(bytes.TrimSpace(lines[i]), &r) == nil && r.Text != "" {
			return r, nil
		}
	}
	return agent.ClaudeResult{}, fmt.Errorf("no result in %s", path)
}

var exitLine = regexp.MustCompile(`\[exit (\d+) after ([^\]]+)\]`)

// loadTestRun rebuilds the test evidence from a saved test.log.
func loadTestRun(path string) (agent.TestRun, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return agent.TestRun{}, err
	}
	m := exitLine.FindAllSubmatch(b, -1)
	if len(m) == 0 {
		return agent.TestRun{}, fmt.Errorf("no exit marker in %s", path)
	}
	last := m[len(m)-1]
	code, _ := strconv.Atoi(string(last[1]))
	d, _ := time.ParseDuration(string(last[2]))
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return agent.TestRun{Passed: code == 0, ExitCode: code, Duration: d, Tail: strings.Join(lines, "\n")}, nil
}
