package scheduler

import (
	"context"
	"strings"

	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"
)

// JobFooter renders the notice footer for an agent job's current status.
func JobFooter(j store.Job) string {
	switch j.Status {
	case store.JobQueued:
		if !j.NotBefore.IsZero() {
			return telegram.FooterRetrying(j.RepoName, j.Attempt, j.NotBefore.Format("15:04"), j.ResumeOf > 0)
		}
		return telegram.FooterQueued(j.RepoName)
	case store.JobRunning:
		return telegram.FooterRunning(j.RepoName)
	case store.JobFailed:
		return telegram.FooterFailed(j.RepoName, j.Error)
	}
	switch {
	case !j.DeliveredAt.IsZero():
		return telegram.FooterDelivered(j.RepoName)
	case !j.MergedAt.IsZero():
		return telegram.FooterMerged(j.RepoName, j.MRURL)
	case j.Error != "": // succeeded without changes in this repo (multi-repo group)
		return telegram.FooterUnchanged(j.RepoName, j.Error)
	default:
		return telegram.FooterDone(j.RepoName, j.MRURL, j.Branch, j.TestsPassed)
	}
}

// GroupFooter renders a job group (one job per repo of the scope). While queued or running
// the group is one line; once finished each repo gets its own line.
func GroupFooter(jobs []store.Job) string {
	if len(jobs) == 1 {
		return JobFooter(jobs[0])
	}
	names := make([]string, len(jobs))
	same := true
	for i, j := range jobs {
		names[i] = j.RepoName
		same = same && j.Status == jobs[0].Status && j.NotBefore.Equal(jobs[0].NotBefore)
	}
	if same && (jobs[0].Status == store.JobQueued || jobs[0].Status == store.JobRunning) {
		g := jobs[0]
		g.RepoName = strings.Join(names, ", ")
		return JobFooter(g)
	}
	lines := make([]string, len(jobs))
	for i, j := range jobs {
		lines[i] = JobFooter(j)
	}
	return strings.Join(lines, "\n")
}

// ApprovedText renders an approved task: its latest job group's progress, or "no repo" when
// none. Approved notices never carry buttons.
func ApprovedText(t store.Task, jobs []store.Job) string {
	footer := telegram.FooterNoRepo()
	if len(jobs) > 0 {
		footer = GroupFooter(jobs)
	}
	return telegram.RenderNotice(notice(t), footer, false)
}

// JobKeyboard offers Retry once the whole group has finished and some repo is not merged yet;
// nil while queued/running (merged fixes are not retried from Telegram).
func JobKeyboard(t store.Task, jobs []store.Job) telegram.Keyboard {
	if len(jobs) == 0 {
		return nil
	}
	open := false
	for _, j := range jobs {
		if j.Status == store.JobQueued || j.Status == store.JobRunning {
			return nil
		}
		open = open || j.MergedAt.IsZero()
	}
	if !open {
		return nil
	}
	return telegram.JobKeyboard(t.JiraKey)
}

// render is view plus job progress for approved tasks.
func (s *Service) render(ctx context.Context, t store.Task) (string, telegram.Keyboard) {
	if t.State != store.StateApproved {
		return s.view(t)
	}
	jobs, err := s.Store.LatestGroup(ctx, t.JiraKey)
	switch {
	case err == nil:
		return ApprovedText(t, jobs), JobKeyboard(t, jobs)
	case store.IsNotFound(err):
		return ApprovedText(t, nil), nil
	default:
		s.Log.Warn("load latest job group", "key", t.JiraKey, "err", err)
		return s.view(t)
	}
}
