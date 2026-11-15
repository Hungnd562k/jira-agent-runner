package worker

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

// errInterrupted is the job error FailOrphaned stores (worker restarted mid-job).
const errInterrupted = "interrupted (worker restarted)"

// transientMarks are error fragments of failures that a later attempt can get past without
// any change: the host is offline, DNS/TLS fails, the API is overloaded or returns 5xx.
var transientMarks = []string{
	errInterrupted, "interrupted (worker stopped)",
	"connection error", "connection refused", "connection reset", "connection timed out",
	"econnrefused", "econnreset", "etimedout", "enotfound", "eai_again", "enetunreach", "ehostunreach",
	"getaddrinfo", "network is unreachable", "no route to host", "temporary failure in name resolution",
	"could not resolve host", "could not read from remote repository", "unable to access",
	"socket hang up", "fetch failed", "tls handshake timeout", "i/o timeout", "client.timeout exceeded",
	"overloaded", "git network step timed out",
}

var transient5xx = regexp.MustCompile(`(?i)\b(api error|status|status code|http)\W{0,3}5\d\d\b`)

// isTransient reports whether a job error looks like a network/API hiccup rather than a
// problem with the fix itself (agent timeout, no changes, failed tests are not transient).
func isTransient(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range transientMarks {
		if strings.Contains(m, s) {
			return true
		}
	}
	return transient5xx.MatchString(msg)
}

// retryDelay is the backoff before automatic attempt n (1-based): RetryDelay·2^(n-1).
func (w *Worker) retryDelay(n int) time.Duration {
	d := w.RetryDelay
	if d <= 0 {
		d = 2 * time.Minute
	}
	for i := 1; i < n && d < 2*time.Hour; i++ {
		d *= 2
	}
	return d
}

// maybeRetry queues the automatic retry of a transiently failed job. stopped = the worker is
// shutting down: the job is requeued without backoff and without using up an attempt.
// Returns the queued job, or ok=false when the failure is final.
func (w *Worker) maybeRetry(ctx context.Context, log *slog.Logger, j store.Job, stopped bool) (store.Job, bool) {
	if j.Status != store.JobFailed || (!stopped && (!isTransient(j.Error) || j.Attempt >= w.AutoRetries)) {
		return store.Job{}, false
	}
	now := w.Now()
	notBefore := now.Add(w.retryDelay(j.Attempt + 1))
	if stopped {
		notBefore = time.Time{}
	}
	next, err := w.Store.AutoRetry(ctx, j, !stopped, notBefore, now)
	if err != nil {
		log.Warn("auto-retry", "err", err)
		return store.Job{}, false
	}
	log.Info("auto-retry queued", "next_job", next.ID, "attempt", next.Attempt, "not_before", notBefore,
		"resume_of", next.ResumeOf)
	if !stopped {
		w.reply(ctx, j.JiraKey, retryText(j, next, w.AutoRetries))
	}
	w.updateNotice(ctx, next)
	return next, true
}

func retryText(failed, next store.Job, max int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔁 <b>Transient failure</b>: %s\nAuto-retry %d/%d at %s",
		html.EscapeString(truncate(failed.Error, 300)), next.Attempt, max, next.NotBefore.Format("15:04"))
	if next.ResumeOf > 0 {
		b.WriteString(" — continuing the partial work")
	}
	return b.String()
}

// wipText tells where a final failure's partial work is kept.
func wipText(j store.Job) string {
	return fmt.Sprintf("💾 Partial work kept on local branch <code>%s</code> (<code>%s</code>). Retry on the same repo continues from it.",
		html.EscapeString(j.Branch), html.EscapeString(shortSHA(j.WIPSHA)))
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func wipMessage(j store.Job, reason string) string {
	return fmt.Sprintf("wip(%s): partial agent work of job %d\n\nNot reviewed, never pushed. Stopped by: %s\n",
		j.JiraKey, j.ID, truncate(reason, 200))
}

// recover runs at worker start, when no job of this worker can be running: it reaps agents
// left behind by a crash, salvages their partial work, removes stale worktrees and queues
// automatic retries for the interrupted jobs.
func (w *Worker) recover(ctx context.Context) error {
	orphans, err := w.Store.FailOrphaned(ctx, w.Now())
	if err != nil {
		return err
	}
	for _, j := range orphans {
		if j.AgentPGID > 0 && j.BootID != "" && j.BootID == w.BootID {
			if err := agent.KillGroup(j.AgentPGID); err != nil {
				w.Log.Warn("reap orphaned agent", "job", j.ID, "pgid", j.AgentPGID, "err", err)
			} else {
				w.Log.Warn("reaped orphaned agent", "job", j.ID, "pgid", j.AgentPGID)
			}
		}
	}
	w.sweepWorktrees(ctx)
	for _, j := range orphans {
		if fresh, err := w.Store.GetJob(ctx, j.ID); err == nil {
			j = fresh // sweep may have saved WIP
		}
		log := w.Log.With("job", j.ID, "key", j.JiraKey, "repo", j.RepoName)
		log.Warn("orphaned job marked failed", "wip", j.WIPSHA != "")
		w.updateNotice(ctx, j)
		if _, ok := w.maybeRetry(ctx, log, j, false); !ok && j.WIPSHA != "" {
			w.reply(ctx, j.JiraKey, wipText(j))
		}
	}
	return nil
}

var jobBranch = regexp.MustCompile(`^agent/.+-j(\d+)$`)

// sweepWorktrees removes every agent worktree still registered in a configured repo (all are
// stale at startup), first committing uncommitted changes as the job's WIP.
func (w *Worker) sweepWorktrees(ctx context.Context) {
	seen := map[string]bool{}
	for _, list := range w.Repos {
		for _, repo := range list {
			if seen[repo.Path] {
				continue
			}
			seen[repo.Path] = true
			g := agent.Git{Dir: repo.Path}
			wts, err := g.Worktrees(ctx)
			if err != nil {
				w.Log.Warn("list worktrees", "repo", repo.Path, "err", err)
				continue
			}
			for _, lw := range wts {
				m := jobBranch.FindStringSubmatch(lw.Branch)
				if m == nil {
					continue // not ours
				}
				id, _ := strconv.ParseInt(m[1], 10, 64)
				w.salvage(ctx, repo.Path, lw, id)
			}
			_, _ = g.Run(ctx, "worktree", "prune")
		}
	}
}

func (w *Worker) salvage(ctx context.Context, repoPath string, lw agent.ListedWorktree, id int64) {
	log := w.Log.With("job", id, "worktree", lw.Dir)
	wt := &agent.Worktree{Repo: agent.Git{Dir: repoPath}, Dir: lw.Dir, Branch: lw.Branch}
	if j, err := w.Store.GetJob(ctx, id); err == nil && j.Status == store.JobFailed && j.WIPSHA == "" {
		if _, statErr := os.Stat(lw.Dir); statErr == nil {
			sha, err := wt.SaveWIP(ctx, wipMessage(j, j.Error))
			switch {
			case err != nil:
				log.Warn("save wip", "err", err)
			case sha != "":
				if err := w.Store.SetJobWIP(ctx, id, sha); err != nil {
					log.Warn("record wip", "err", err)
				}
				log.Info("salvaged partial work", "sha", shortSHA(sha))
			}
		}
	}
	if err := wt.Remove(ctx); err != nil {
		log.Warn("remove stale worktree", "err", err)
		return
	}
	log.Info("removed stale worktree")
}

// netCtx bounds a git network step (fetch/push) so a dead connection cannot stall the queue.
func (w *Worker) netCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	d := w.GitNetTimeout
	if d <= 0 {
		d = 5 * time.Minute
	}
	return context.WithTimeout(ctx, d)
}

// netErr labels a git network failure caused by GitNetTimeout so it is classified as transient.
func netErr(nctx context.Context, err error) error {
	if err != nil && nctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("git network step timed out: %w", err)
	}
	return err
}
