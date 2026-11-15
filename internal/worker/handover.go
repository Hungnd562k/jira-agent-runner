package worker

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"
)

// HandoverConfig controls batching fixes merged into dev into one dev → test handover.
type HandoverConfig struct {
	MinTickets   int           // propose when at least this many tickets wait (default 1)
	PollEvery    time.Duration // CI/CD status poll (default 1m)
	StartTimeout time.Duration // a pipeline must appear for the merge commit within this (default 15m)
	Timeout      time.Duration // give up watching CI/CD after this (default 90m)
	JiraStatus   string        // workflow status reached after a successful handover (default "Resolved")
	Resolution   string        // resolution sent when the transition asks for one (default "Fixed")
}

func (c HandoverConfig) withDefaults() HandoverConfig {
	if c.MinTickets <= 0 {
		c.MinTickets = 1
	}
	if c.PollEvery <= 0 {
		c.PollEvery = time.Minute
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 15 * time.Minute
	}
	if c.Timeout <= 0 {
		c.Timeout = 90 * time.Minute
	}
	if c.JiraStatus == "" {
		c.JiraStatus = "Resolved"
	}
	if c.Resolution == "" {
		c.Resolution = "Fixed"
	}
	return c
}

// batch is one repo's share of a handover.
type batch struct {
	repo    repos.Repo
	jobs    []store.Job
	foreign []string // commits in base..handover not made by the agent for these tickets
	scanErr string
}

// repoByName maps repo name → config for the given jobs.
func (w *Worker) repoByName(jobs []store.Job) map[string]repos.Repo {
	out := map[string]repos.Repo{}
	for _, j := range jobs {
		if r, ok := w.Repos.Find(j.JiraKey, j.RepoName); ok {
			out[j.RepoName] = r
		}
	}
	return out
}

// proposeHandover asks on Telegram whether to hand over the fixes waiting in dev. It only
// asks when no job is queued/running, no handover is in flight or snoozed, and at least
// MinTickets tickets wait.
func (w *Worker) proposeHandover(ctx context.Context) error {
	cfg := w.Handover.withDefaults()
	now := w.Now()
	if n, err := w.Store.ActiveJobCount(ctx); err != nil || n > 0 {
		return err
	}
	if _, blocked, err := w.Store.BlockingHandover(ctx, now); err != nil || blocked {
		return err
	}
	pending, err := w.Store.PendingHandoverJobs(ctx)
	if err != nil || len(pending) == 0 {
		return err
	}
	cfgs := w.repoByName(pending)
	byRepo := map[string]*batch{}
	tickets := map[string]bool{}
	for _, j := range pending {
		r, ok := cfgs[j.RepoName]
		if !ok || r.HandoverBranch == "" {
			continue
		}
		b := byRepo[j.RepoName]
		if b == nil {
			b = &batch{repo: r}
			byRepo[j.RepoName] = b
		}
		b.jobs = append(b.jobs, j)
		tickets[j.JiraKey] = true
	}
	if len(tickets) < cfg.MinTickets {
		return nil
	}
	names := make([]string, 0, len(byRepo))
	for n := range byRepo {
		names = append(names, n)
	}
	sort.Strings(names)
	var (
		batches []batch
		ids     []int64
		hrs     []store.HandoverRepo
	)
	for _, n := range names {
		b := byRepo[n]
		w.scanCommits(ctx, b)
		batches = append(batches, *b)
		for _, j := range b.jobs {
			ids = append(ids, j.ID)
		}
		hrs = append(hrs, store.HandoverRepo{RepoName: n, Project: b.repo.GitLabProject})
	}
	h, err := w.Store.ProposeHandover(ctx, ids, hrs, now)
	if err != nil {
		return err
	}
	text := w.questionHTML(ctx, h.ID, batches)
	msgID, err := w.TG.SendMessage(ctx, w.ChatID, text, telegram.HandoverKeyboard(h.ID), 0)
	if err != nil {
		_ = w.Store.SetHandoverStatus(ctx, h.ID, store.HandoverFailed, "telegram: "+err.Error(), time.Time{}, true, now)
		return fmt.Errorf("send handover question: %w", err)
	}
	w.Log.Info("handover proposed", "id", h.ID, "tickets", len(tickets), "repos", names)
	return w.Store.SetHandoverMessage(ctx, h.ID, msgID, text)
}

// scanCommits lists commits in origin/<base> that are not in origin/<handover> and were not
// made for the batch's tickets (other developers' work that rides along).
func (w *Worker) scanCommits(ctx context.Context, b *batch) {
	g := agent.Git{Dir: b.repo.Path}
	if _, err := g.Run(ctx, "fetch", "--quiet", "origin", b.repo.BaseBranch, b.repo.HandoverBranch); err != nil {
		b.scanErr = err.Error()
		return
	}
	out, err := g.Run(ctx, "log", "--no-merges", "--format=%h%x09%an%x09%s",
		"origin/"+b.repo.HandoverBranch+"..origin/"+b.repo.BaseBranch)
	if err != nil {
		b.scanErr = err.Error()
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 3 && ours(parts[2], b.jobs) {
			continue
		}
		b.foreign = append(b.foreign, strings.ReplaceAll(line, "\t", " · "))
	}
}

// ours: the agent's commit/MR subjects are "fix(<KEY>): …" or "[<KEY>] …".
func ours(subject string, jobs []store.Job) bool {
	for _, j := range jobs {
		if strings.HasPrefix(subject, "fix("+j.JiraKey+")") || strings.HasPrefix(subject, "["+j.JiraKey+"]") {
			return true
		}
	}
	return false
}

func (w *Worker) questionHTML(ctx context.Context, id int64, batches []batch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚀 <b>Bàn giao lên test?</b> (#%d)\n", id)
	for _, bt := range batches {
		fmt.Fprintf(&b, "\n📦 <b>%s</b>: <code>%s</code> → <code>%s</code>\n", html.EscapeString(bt.repo.Name),
			html.EscapeString(bt.repo.BaseBranch), html.EscapeString(bt.repo.HandoverBranch))
		seen := map[string]bool{}
		for _, j := range bt.jobs {
			if seen[j.JiraKey] {
				continue
			}
			seen[j.JiraKey] = true
			b.WriteString("• " + w.ticketLink(ctx, j.JiraKey) + "\n")
		}
		switch {
		case bt.scanErr != "":
			b.WriteString("⚠️ Không đọc được danh sách commit: " + html.EscapeString(truncate(bt.scanErr, 200)) + "\n")
		case len(bt.foreign) > 0:
			fmt.Fprintf(&b, "⚠️ Kèm theo <b>%d commit khác</b> trong <code>%s</code> chưa có ở <code>%s</code>:\n<pre>",
				len(bt.foreign), html.EscapeString(bt.repo.BaseBranch), html.EscapeString(bt.repo.HandoverBranch))
			for i, c := range bt.foreign {
				if i == 10 {
					fmt.Fprintf(&b, "… +%d nữa\n", len(bt.foreign)-10)
					break
				}
				b.WriteString(html.EscapeString(truncate(c, 90)) + "\n")
			}
			b.WriteString("</pre>")
		default:
			b.WriteString("✔️ Không có commit nào khác ngoài các ticket trên.\n")
		}
	}
	b.WriteString("\nBấm <b>Có</b>: tạo MR, tự approve và merge sang test. CI/CD tự chạy; mình sẽ theo dõi và báo kết quả.")
	return b.String()
}

func (w *Worker) ticketLink(ctx context.Context, key string) string {
	t, err := w.Store.Get(ctx, key)
	if err != nil || t.URL == "" {
		return "<b>" + html.EscapeString(key) + "</b>"
	}
	return fmt.Sprintf(`<a href="%s">%s</a> %s`, html.EscapeString(t.URL), html.EscapeString(key), html.EscapeString(truncate(t.Summary, 80)))
}

// runApprovedHandover creates (or reuses), approves and merges the base → handover MR of
// every repo in the oldest approved handover, then starts watching CI/CD.
func (w *Worker) runApprovedHandover(ctx context.Context) {
	h, ok, err := w.Store.NextApprovedHandover(ctx)
	if err != nil {
		w.Log.Error("load approved handover", "err", err)
		return
	}
	if !ok {
		return
	}
	cfg := w.Handover.withDefaults()
	log := w.Log.With("handover", h.ID)
	names := make([]string, 0, len(h.Repos))
	for _, r := range h.Repos {
		names = append(names, r.RepoName)
	}
	// Fixes merged after the question ride along in the same merge: report them too.
	if err := w.Store.AttachPendingJobs(ctx, h.ID, names); err != nil {
		log.Warn("attach pending jobs", "err", err)
	}
	if h, err = w.Store.GetHandover(ctx, h.ID); err != nil {
		log.Error("reload handover", "err", err)
		return
	}
	cfgs := w.repoByName(h.Jobs)
	var merged int
	var errs []string
	for i := range h.Repos {
		r := &h.Repos[i]
		if r.Status == store.HRMerged || r.Status == store.HRSuccess { // resumed after a restart
			merged++
			continue
		}
		rc, ok := cfgs[r.RepoName]
		if !ok {
			r.Status, r.Error = store.HRFailed, "repo not found in repos.json"
		} else if err := w.mergeHandoverRepo(ctx, log, h, r, rc); err != nil {
			r.Status, r.Error = store.HRFailed, err.Error()
		}
		if err := w.Store.UpdateHandoverRepo(ctx, *r); err != nil {
			log.Error("save handover repo", "err", err)
		}
		if r.Status == store.HRMerged {
			merged++
			continue
		}
		errs = append(errs, r.RepoName+": "+r.Error)
		if err := w.Store.DetachRepoJobs(ctx, h.ID, r.RepoName); err != nil {
			log.Warn("detach jobs", "err", err)
		}
	}
	now := w.Now()
	if merged == 0 {
		_ = w.Store.SetHandoverStatus(ctx, h.ID, store.HandoverFailed, strings.Join(errs, "; "), time.Time{}, true, now)
		w.handoverReply(ctx, h, "❌ <b>Không merge được sang test</b> (#"+fmt.Sprint(h.ID)+")\n"+htmlLines(errs)+
			"\nCác ticket vẫn chờ bàn giao; mình sẽ hỏi lại khi queue rảnh.", nil)
		log.Warn("handover failed before merge", "errors", errs)
		return
	}
	if err := w.Store.SetHandoverStatus(ctx, h.ID, store.HandoverDeploying, "", now.Add(cfg.Timeout), false, now); err != nil {
		log.Error("set deploying", "err", err)
		return
	}
	h, _ = w.Store.GetHandover(ctx, h.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "🔀 <b>Đã merge sang test</b> (#%d) — đang theo dõi CI/CD…\n", h.ID)
	for _, r := range h.Repos {
		if r.Status == store.HRMerged {
			fmt.Fprintf(&b, "• <code>%s</code>: <a href=\"%s\">MR !%d</a>\n", html.EscapeString(r.RepoName), html.EscapeString(r.MRURL), r.MRIID)
		}
	}
	if len(errs) > 0 {
		b.WriteString("⚠️ Không merge được (ticket của repo này vẫn chờ bàn giao):\n" + htmlLines(errs))
	}
	w.handoverReply(ctx, h, b.String(), nil)
	log.Info("handover merged; watching CI/CD", "repos_merged", merged)
}

func (w *Worker) mergeHandoverRepo(ctx context.Context, log *slog.Logger, h store.Handover, r *store.HandoverRepo, rc repos.Repo) error {
	if rc.HandoverBranch == "" {
		return fmt.Errorf("handover_branch not configured")
	}
	mr, found, err := w.MR.FindOpenMR(ctx, rc.GitLabProject, rc.BaseBranch, rc.HandoverBranch)
	if err != nil {
		return err
	}
	if !found {
		var keys []string
		var desc strings.Builder
		fmt.Fprintf(&desc, "Bàn giao `%s` → `%s` (jira-agent-runner #%d).\n\nTicket:\n", rc.BaseBranch, rc.HandoverBranch, h.ID)
		seen := map[string]bool{}
		for _, j := range h.Jobs {
			if j.RepoName != rc.Name || seen[j.JiraKey] {
				continue
			}
			seen[j.JiraKey] = true
			keys = append(keys, j.JiraKey)
			fmt.Fprintf(&desc, "- %s — %s\n", j.JiraKey, j.MRURL)
		}
		desc.WriteString("\nMR này mang theo mọi commit hiện có trong `" + rc.BaseBranch + "`.\n\n" + coAuthor + "\n")
		mr, err = w.MR.OpenMR(ctx, MRRequest{
			Repo:         rc,
			SourceBranch: rc.BaseBranch,
			Target:       rc.HandoverBranch,
			Title:        "Bàn giao " + rc.HandoverBranch + ": " + strings.Join(keys, ", "),
			Description:  desc.String(),
			KeepSource:   true,
		})
		if err != nil {
			return err
		}
	}
	r.MRIID, r.MRURL = mr.IID, mr.WebURL
	if err := w.MR.ApproveMR(ctx, rc.GitLabProject, mr.IID); err != nil {
		log.Warn("approve handover MR (merging anyway)", "repo", rc.Name, "err", err)
	}
	sha, err := w.MR.MergeMR(ctx, rc.GitLabProject, mr.IID, false)
	if err != nil {
		return err
	}
	r.MergeSHA, r.Status, r.Error = sha, store.HRMerged, ""
	return nil
}

// watchLoop polls CI/CD of deploying handovers until ctx is done.
func (w *Worker) watchLoop(ctx context.Context) {
	t := time.NewTicker(w.Handover.withDefaults().PollEvery)
	defer t.Stop()
	for {
		w.WatchOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// WatchOnce checks the pipelines of every deploying handover once.
func (w *Worker) WatchOnce(ctx context.Context) {
	hs, err := w.Store.HandoversIn(ctx, store.HandoverDeploying)
	if err != nil {
		w.Log.Error("load deploying handovers", "err", err)
		return
	}
	for _, h := range hs {
		w.watchHandover(ctx, h)
	}
}

var pipelineDone = map[string]bool{"success": true, "failed": true, "canceled": true, "skipped": true, "manual": true}

func (w *Worker) watchHandover(ctx context.Context, h store.Handover) {
	cfg := w.Handover.withDefaults()
	log := w.Log.With("handover", h.ID)
	now := w.Now()
	cfgs := w.repoByName(h.Jobs)
	for i := range h.Repos {
		r := &h.Repos[i]
		if r.Status != store.HRMerged {
			continue
		}
		rc, ok := cfgs[r.RepoName]
		if !ok {
			r.Status, r.Error = store.HRFailed, "repo not found in repos.json"
		} else {
			w.pollRepo(ctx, log, h, r, rc, cfg, now)
		}
		if err := w.Store.UpdateHandoverRepo(ctx, *r); err != nil {
			log.Error("save handover repo", "err", err)
		}
	}
	running, failed := 0, 0
	for _, r := range h.Repos {
		switch r.Status {
		case store.HRMerged:
			running++
		case store.HRFailed:
			if r.MergeSHA != "" {
				failed++
			}
		}
	}
	if running > 0 && !h.DeadlineAt.IsZero() && now.After(h.DeadlineAt) {
		for i := range h.Repos {
			if r := &h.Repos[i]; r.Status == store.HRMerged {
				r.Status, r.Error = store.HRFailed, fmt.Sprintf("hết thời gian theo dõi (%s), pipeline chưa xong", cfg.Timeout)
				_ = w.Store.UpdateHandoverRepo(ctx, *r)
			}
		}
		running, failed = 0, failed+1
	}
	if running > 0 {
		return
	}
	if failed > 0 {
		w.finishFailed(ctx, log, h)
		return
	}
	w.finishSucceeded(ctx, log, h)
}

func (w *Worker) pollRepo(ctx context.Context, log *slog.Logger, h store.Handover, r *store.HandoverRepo, rc repos.Repo, cfg HandoverConfig, now time.Time) {
	if r.PipelineID == 0 {
		p, ok, err := w.MR.FindPipeline(ctx, rc.GitLabProject, rc.HandoverBranch, r.MergeSHA)
		switch {
		case err != nil:
			log.Warn("find pipeline", "repo", r.RepoName, "err", err)
			return
		case !ok:
			if now.Sub(h.UpdatedAt) > cfg.StartTimeout {
				r.Status, r.Error = store.HRFailed, fmt.Sprintf("không thấy pipeline nào cho commit merge %.8s sau %s", r.MergeSHA, cfg.StartTimeout)
			}
			return
		}
		r.PipelineID, r.PipelineURL = p.ID, p.WebURL
	}
	p, err := w.MR.GetPipeline(ctx, rc.GitLabProject, r.PipelineID)
	if err != nil {
		log.Warn("get pipeline", "repo", r.RepoName, "err", err)
		return
	}
	switch {
	case p.Status == "success":
		r.Status, r.Error = store.HRSuccess, ""
	case pipelineDone[p.Status]:
		// A newer pipeline on the branch (retry from GitLab, later merge) supersedes this one.
		if newer, ok, err := w.MR.FindPipeline(ctx, rc.GitLabProject, rc.HandoverBranch, ""); err == nil && ok && newer.ID > r.PipelineID {
			r.PipelineID, r.PipelineURL = newer.ID, newer.WebURL
			return
		}
		r.Status, r.Error = store.HRFailed, w.pipelineFailure(ctx, rc.GitLabProject, p)
	}
}

// pipelineFailure explains why a pipeline did not succeed: failed jobs and the log tail of the first.
func (w *Worker) pipelineFailure(ctx context.Context, project string, p Pipeline) string {
	if p.Status == "manual" {
		return "pipeline dừng ở job manual (cần bấm chạy trên GitLab)"
	}
	if p.Status != "failed" {
		return "pipeline " + p.Status
	}
	jobs, err := w.MR.FailedJobs(ctx, project, p.ID)
	if err != nil || len(jobs) == 0 {
		return "pipeline failed (không lấy được danh sách job lỗi)"
	}
	var b strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&b, "job %s (stage %s) failed", j.Name, j.Stage)
		if j.FailureReason != "" {
			b.WriteString(": " + j.FailureReason)
		}
		b.WriteString("\n")
	}
	if tail, err := w.MR.JobLogTail(ctx, project, jobs[0].ID, 25); err == nil && tail != "" {
		b.WriteString("--- log " + jobs[0].Name + " ---\n" + tail)
	}
	return b.String()
}

func (w *Worker) finishSucceeded(ctx context.Context, log *slog.Logger, h store.Handover) {
	cfg := w.Handover.withDefaults()
	cfgs := w.repoByName(h.Jobs)
	repoOf := map[string]store.HandoverRepo{}
	for _, r := range h.Repos {
		repoOf[r.RepoName] = r
	}
	var lines []string
	seen := map[string]bool{}
	for _, j := range h.Jobs {
		if seen[j.JiraKey] {
			continue
		}
		seen[j.JiraKey] = true
		lines = append(lines, w.deliver(ctx, log, j, repoOf[j.RepoName], cfgs[j.RepoName], cfg))
	}
	now := w.Now()
	if err := w.Store.SetHandoverStatus(ctx, h.ID, store.HandoverSucceeded, "", time.Time{}, false, now); err != nil {
		log.Error("set succeeded", "err", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ <b>CI/CD test thành công</b> — bàn giao #%d\n", h.ID)
	for _, r := range h.Repos {
		if r.Status == store.HRSuccess {
			fmt.Fprintf(&b, "• <code>%s</code>: <a href=\"%s\">MR !%d</a> · <a href=\"%s\">pipeline #%d</a>\n", html.EscapeString(r.RepoName),
				html.EscapeString(r.MRURL), r.MRIID, html.EscapeString(r.PipelineURL), r.PipelineID)
		}
	}
	b.WriteString("\n<b>Ticket đã bàn giao</b>\n" + strings.Join(lines, "\n"))
	w.handoverReply(ctx, h, b.String(), nil)
	for _, j := range h.Jobs {
		if fresh, err := w.Store.GetJob(ctx, j.ID); err == nil {
			w.updateNotice(ctx, fresh.JiraKey)
		}
	}
	log.Info("handover delivered", "tickets", len(lines))
}

// deliver comments on the ticket, moves it to the configured status and back to its reporter.
// Returns the Telegram line for the ticket.
func (w *Worker) deliver(ctx context.Context, log *slog.Logger, j store.Job, r store.HandoverRepo, rc repos.Repo, cfg HandoverConfig) string {
	link := w.ticketLink(ctx, j.JiraKey)
	var problems []string
	if w.Comment != nil {
		body := fmt.Sprintf("(/) *Đã bàn giao lên {{%s}}* (repo %s): MR %s, CI/CD pipeline %s thành công.\n\n{{%s}}",
			rc.HandoverBranch, r.RepoName, r.MRURL, r.PipelineURL, coAuthor)
		if err := w.Comment.AddComment(ctx, j.JiraKey, body); err != nil {
			problems = append(problems, "comment: "+err.Error())
		}
	}
	state := ""
	if w.Resolver != nil {
		to, err := w.Resolver.Transition(ctx, j.JiraKey, cfg.JiraStatus, cfg.Resolution)
		if err != nil {
			problems = append(problems, "transition: "+err.Error())
		} else {
			state = to
		}
		if name, err := w.Resolver.AssignToReporter(ctx, j.JiraKey); err != nil {
			problems = append(problems, "assign: "+err.Error())
		} else {
			state += ", giao lại cho " + name
		}
	}
	if len(problems) > 0 {
		log.Warn("deliver ticket", "key", j.JiraKey, "problems", problems)
		return "⚠️ " + link + " — " + html.EscapeString(truncate(strings.Join(problems, "; "), 300))
	}
	if state = strings.TrimPrefix(state, ", "); state == "" {
		return "✅ " + link
	}
	return "✅ " + link + " — " + html.EscapeString(state)
}

func (w *Worker) finishFailed(ctx context.Context, log *slog.Logger, h store.Handover) {
	var summary []string
	var b strings.Builder
	fmt.Fprintf(&b, "❌ <b>CI/CD test thất bại</b> — bàn giao #%d\n", h.ID)
	for _, r := range h.Repos {
		if r.MergeSHA == "" {
			continue
		}
		fmt.Fprintf(&b, "\n📦 <code>%s</code> <a href=\"%s\">MR !%d</a>", html.EscapeString(r.RepoName), html.EscapeString(r.MRURL), r.MRIID)
		if r.PipelineURL != "" {
			fmt.Fprintf(&b, " · <a href=\"%s\">pipeline #%d</a>", html.EscapeString(r.PipelineURL), r.PipelineID)
		}
		if r.Status == store.HRSuccess {
			b.WriteString(" ✅\n")
			continue
		}
		b.WriteString(" ❌\n<pre>" + html.EscapeString(truncate(r.Error, 1500)) + "</pre>\n")
		summary = append(summary, r.RepoName+": "+firstLineOf(r.Error))
	}
	b.WriteString("\nCode đã ở nhánh test; ticket chưa được resolve. Sửa/retry pipeline trên GitLab rồi bấm <b>Theo dõi lại</b>.")
	if err := w.Store.SetHandoverStatus(ctx, h.ID, store.HandoverFailed, strings.Join(summary, "; "), time.Time{}, false, w.Now()); err != nil {
		log.Error("set failed", "err", err)
	}
	w.handoverReply(ctx, h, b.String(), telegram.RewatchKeyboard(h.ID))
	log.Warn("handover CI/CD failed", "summary", summary)
}

func (w *Worker) handoverReply(ctx context.Context, h store.Handover, text string, kb telegram.Keyboard) {
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	if _, err := w.TG.SendMessage(ctx, w.ChatID, text, kb, h.TGMessageID); err != nil {
		w.Log.Warn("send handover message", "handover", h.ID, "err", err)
	}
}

func htmlLines(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("• " + html.EscapeString(truncate(l, 300)) + "\n")
	}
	return b.String()
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return truncate(s, 200)
}
