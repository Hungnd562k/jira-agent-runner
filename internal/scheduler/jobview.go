package scheduler

import (
	"context"

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
	default:
		return telegram.FooterDone(j.RepoName, j.MRURL, j.Branch, j.TestsPassed)
	}
}

// ApprovedText renders an approved task: its latest job's progress, or "no repo" when none.
// Approved notices never carry buttons.
func ApprovedText(t store.Task, j *store.Job) string {
	footer := telegram.FooterNoRepo()
	if j != nil {
		footer = JobFooter(*j)
	}
	return telegram.RenderNotice(notice(t), footer, false)
}

// JobKeyboard offers Retry / Other repo once the job has finished; nil while queued/running.
func JobKeyboard(t store.Task, j *store.Job) telegram.Keyboard {
	if j == nil || j.Status == store.JobQueued || j.Status == store.JobRunning || !j.MergedAt.IsZero() {
		return nil // merged fixes are not retried from Telegram
	}
	return telegram.JobKeyboard(t.JiraKey)
}

// render is view plus job progress for approved tasks.
func (s *Service) render(ctx context.Context, t store.Task) (string, telegram.Keyboard) {
	if t.State != store.StateApproved {
		return s.view(t)
	}
	j, err := s.Store.LatestJob(ctx, t.JiraKey)
	switch {
	case err == nil:
		return ApprovedText(t, &j), JobKeyboard(t, &j)
	case store.IsNotFound(err):
		return ApprovedText(t, nil), nil
	default:
		s.Log.Warn("load latest job", "key", t.JiraKey, "err", err)
		return s.view(t)
	}
}
