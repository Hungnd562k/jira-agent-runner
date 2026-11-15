package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/scope"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"
)

// Messenger is the Telegram surface the service uses.
type Messenger interface {
	SendMessage(ctx context.Context, chatID int64, html string, kb telegram.Keyboard, replyTo int64) (int64, error)
	EditMessage(ctx context.Context, chatID, messageID int64, html string, kb telegram.Keyboard) error
	AnswerCallback(ctx context.Context, callbackID, text string) error
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]telegram.Update, error)
}

// IssueGetter loads an issue with its comments (to read the maintainer's scope comment).
type IssueGetter interface {
	GetIssue(ctx context.Context, key string) (jira.IssueDetail, error)
}

// Service runs sync → notify cycles and handles Telegram button presses.
type Service struct {
	Syncer        *Syncer
	Store         *store.Store
	TG            Messenger
	ChatID        int64
	SnoozeChoices []time.Duration
	Repos         repos.Config // candidate repos per Jira project for "Fix it"
	Issues        IssueGetter  // reads the scope comment on "Fix it" / "Retry"
	Scope         scope.Format // format of the maintainer's scope comment (scope.json)
	PollInterval  time.Duration
	RemindAfter   time.Duration // re-remind notified tasks untouched this long; also "later" for handovers
	// HandoverTimeout bounds CI/CD watching after "watch again" (default 90m).
	HandoverTimeout time.Duration
	RemindCheck     time.Duration // how often due reminders are checked (default 1m)
	SendGap         time.Duration // pause between outgoing messages (Telegram: ~1 msg/s per chat)
	Heartbeat       func()        // optional; called each loop tick to prove the main loop is alive
	Now             func() time.Time
	Log             *slog.Logger

	mu sync.Mutex // serializes read-modify-edit of a task's message
}

// Run polls Jira every PollInterval and listens for Telegram updates until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { s.listen(ctx) })

	check := s.RemindCheck
	if check <= 0 {
		check = time.Minute
	}

	beat := func() {
		if s.Heartbeat != nil {
			s.Heartbeat()
		}
	}

	beat()
	s.RefreshApproved(ctx)
	s.runCycle(ctx)
	poll := time.NewTicker(s.PollInterval)
	defer poll.Stop()
	remind := time.NewTicker(check)
	defer remind.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-poll.C:
			s.runCycle(ctx)
		case <-remind.C:
			beat()
			if err := s.Remind(ctx); err != nil && ctx.Err() == nil {
				s.Log.Error("remind failed", "err", err)
			}
		}
	}
}

// Remind re-sends notices that are due (untouched for RemindAfter, or snooze expired)
// as a reply to the previous message, and strips the buttons from that older message.
func (s *Service) Remind(ctx context.Context) error {
	due, err := s.Store.ListDue(ctx, s.Now(), s.RemindAfter)
	if err != nil {
		return err
	}
	for i, t := range due {
		if i > 0 && !sleep(ctx, s.SendGap) {
			return ctx.Err()
		}
		s.withLock(func() { s.remindOne(ctx, t.JiraKey) })
	}
	return nil
}

func (s *Service) remindOne(ctx context.Context, key string) {
	// Re-read under the lock: a button press may have changed the task since ListDue.
	t, err := s.Store.Get(ctx, key)
	if err != nil || !store.IsDue(t, s.Now(), s.RemindAfter) {
		return
	}
	old := t.TGMessageID
	if err := s.send(ctx, t, telegram.FooterReminder(), old); err != nil {
		s.Log.Error("send reminder", "key", key, "err", err)
		return
	}
	s.Log.Info("reminder sent", "key", key, "was", t.State)
	if old != 0 {
		text := telegram.RenderNotice(notice(t), telegram.FooterSuperseded(), false)
		if err := s.TG.EditMessage(ctx, s.ChatID, old, text, nil); err != nil && !telegram.IsNotModified(err) {
			s.Log.Warn("strip old buttons", "key", key, "msg", old, "err", err)
		}
	}
}

func (s *Service) runCycle(ctx context.Context) {
	if err := s.Cycle(ctx); err != nil && ctx.Err() == nil {
		s.Log.Error("cycle failed", "err", err)
	}
}

// Cycle syncs Jira, refreshes changed/closed notices and sends notices for new tasks.
func (s *Service) Cycle(ctx context.Context) error {
	res, err := s.Syncer.Sync(ctx)
	if err != nil {
		return err
	}
	s.Log.Info("sync done", "issues", len(res.Issues), "created", len(res.Created),
		"updated", len(res.Updated), "reopened", len(res.Reopened), "closed", len(res.Closed))

	for _, t := range res.Closed {
		if t.TGMessageID == 0 {
			continue
		}
		t.State = store.StateClosed
		s.withLock(func() { s.refresh(ctx, t, t.TGMessageID) })
	}
	for _, key := range res.Updated {
		s.withLock(func() {
			if t, err := s.Store.Get(ctx, key); err == nil && t.TGMessageID != 0 {
				s.refresh(ctx, t, t.TGMessageID)
			}
		})
	}

	pending, err := s.Store.ListByState(ctx, store.StateNew)
	if err != nil {
		return err
	}
	for i, t := range pending {
		if i > 0 && !sleep(ctx, s.SendGap) {
			return ctx.Err()
		}
		s.withLock(func() {
			if err := s.send(ctx, t, "", 0); err != nil {
				s.Log.Error("send notice", "key", t.JiraKey, "err", err)
			}
		})
	}
	return nil
}

// send posts a fresh notice for t and records it as notified.
func (s *Service) send(ctx context.Context, t store.Task, footer string, replyTo int64) error {
	text := telegram.RenderNotice(notice(t), footer, false)
	id, err := s.TG.SendMessage(ctx, s.ChatID, text, telegram.MainKeyboard(t.JiraKey), replyTo)
	if err != nil {
		return err
	}
	_, err = s.Store.MarkNotified(ctx, t.JiraKey, id, s.Now())
	return err
}

// refresh re-renders messageID to reflect t's current state.
func (s *Service) refresh(ctx context.Context, t store.Task, messageID int64) {
	text, kb := s.render(ctx, t)
	if err := s.TG.EditMessage(ctx, s.ChatID, messageID, text, kb); err != nil && !telegram.IsNotModified(err) {
		s.Log.Warn("edit message", "key", t.JiraKey, "msg", messageID, "err", err)
	}
}

// view maps a task state to message text + keyboard.
func (s *Service) view(t store.Task) (string, telegram.Keyboard) {
	n := notice(t)
	switch t.State {
	case store.StateSnoozed:
		return telegram.RenderNotice(n, telegram.FooterSnoozed(t.RemindAt), false), telegram.MainKeyboard(t.JiraKey)
	case store.StateApproved:
		return telegram.RenderNotice(n, telegram.FooterApproved(t.UpdatedAt), false), nil
	case store.StateClosed:
		return telegram.RenderNotice(n, telegram.FooterClosed(), true), nil
	default:
		return telegram.RenderNotice(n, "", false), telegram.MainKeyboard(t.JiraKey)
	}
}

func notice(t store.Task) telegram.Notice {
	return telegram.Notice{Key: t.JiraKey, Summary: t.Summary, Priority: t.Priority, Status: t.JiraStatus, URL: t.URL}
}

// listen long-polls Telegram and dispatches updates until ctx is done.
func (s *Service) listen(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		ups, err := s.TG.GetUpdates(ctx, offset, 30*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				s.Log.Warn("getUpdates", "err", err)
				sleep(ctx, 5*time.Second)
			}
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			s.HandleUpdate(ctx, u)
		}
	}
}

// HandleUpdate processes one Telegram update. Updates from other chats are ignored.
func (s *Service) HandleUpdate(ctx context.Context, u telegram.Update) {
	switch {
	case u.CallbackQuery != nil:
		s.handleCallback(ctx, u.CallbackQuery)
	case u.Message != nil && u.Message.Chat.ID == s.ChatID && u.Message.Text == "/start":
		_, err := s.TG.SendMessage(ctx, s.ChatID, "👋 jira-agent-runner is running. New Jira issues will show up here.", nil, 0)
		if err != nil {
			s.Log.Warn("reply /start", "err", err)
		}
	}
}

func (s *Service) handleCallback(ctx context.Context, q *telegram.CallbackQuery) {
	answer := func(text string) {
		if err := s.TG.AnswerCallback(ctx, q.ID, text); err != nil {
			s.Log.Warn("answer callback", "err", err)
		}
	}
	if q.Message == nil || q.Message.Chat.ID != s.ChatID {
		s.Log.Warn("callback from unexpected chat ignored")
		answer("Not allowed")
		return
	}
	cb, err := telegram.ParseCallback(q.Data)
	if err != nil {
		answer("Unknown action")
		return
	}
	msgID := q.Message.MessageID
	switch cb.Action {
	case telegram.ActHandoverYes, telegram.ActHandoverLater, telegram.ActHandoverRewatch:
		answer(s.handleHandover(ctx, cb, msgID))
		return
	}

	// Fix it / Retry need the maintainer's scope comment: read it before taking the lock
	// (a Jira round-trip must not block other buttons).
	var sc scopeCheck
	if cb.Action == telegram.ActFix || cb.Action == telegram.ActRetry {
		if sc = s.checkScope(ctx, cb.Key); sc.err != nil {
			s.Log.Warn("read scope comment", "key", cb.Key, "err", sc.err)
			answer("Không đọc được ticket từ Jira, thử lại")
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.Now()
	var (
		t     store.Task
		toast string
	)
	switch {
	case cb.Action == telegram.ActFix:
		if t, err = s.Store.Get(ctx, cb.Key); err != nil {
			break
		}
		if !approvable(t.State) {
			err = store.ErrInvalidTransition
			break
		}
		switch {
		case !sc.configured:
			t, err = s.Store.Approve(ctx, cb.Key, now)
			toast = "✅ Approved (no repo configured)"
		case sc.scope == nil:
			s.askScope(ctx, t, msgID, sc)
			answer("📝 Cần phạm vi sửa từ maintainer — xem hướng dẫn")
			return
		default:
			t, _, err = s.Store.ApproveAndEnqueueRepos(ctx, cb.Key, sc.scope.Repos, now)
			toast = "🕓 Queued on " + strings.Join(sc.scope.Repos, ", ")
		}
	case cb.Action == telegram.ActRetry:
		if t, err = s.Store.Get(ctx, cb.Key); err != nil {
			break
		}
		if t.State != store.StateApproved || !sc.configured {
			err = store.ErrInvalidTransition
			break
		}
		if sc.scope == nil {
			s.askScope(ctx, t, msgID, sc)
			answer("📝 Cần phạm vi sửa từ maintainer — xem hướng dẫn")
			return
		}
		todo := s.retryRepos(ctx, cb.Key, sc.scope.Repos)
		if len(todo) == 0 {
			answer("Mọi repo trong phạm vi đã merge — không còn gì để chạy lại")
			return
		}
		t, err = s.requeue(ctx, cb.Key, todo, now)
		toast = "🔁 Retrying on " + strings.Join(todo, ", ")
	case cb.Action == telegram.ActSnooze && cb.Snooze > 0:
		t, err = s.Store.Snooze(ctx, cb.Key, now.Add(cb.Snooze), now)
		toast = "⏰ Snoozed " + telegram.FormatDuration(cb.Snooze)
	case cb.Action == telegram.ActSnoozeMenu, cb.Action == telegram.ActBack:
		if t, err = s.Store.Get(ctx, cb.Key); err == nil {
			text, kb := s.render(ctx, t)
			if cb.Action == telegram.ActSnoozeMenu && (t.State == store.StateNotified || t.State == store.StateSnoozed) {
				kb = telegram.SnoozeKeyboard(t.JiraKey, s.SnoozeChoices)
			}
			if err := s.TG.EditMessage(ctx, s.ChatID, msgID, text, kb); err != nil && !telegram.IsNotModified(err) {
				s.Log.Warn("edit message", "key", t.JiraKey, "err", err)
			}
			answer("")
			return
		}
	}

	switch {
	case errors.Is(err, store.ErrJobActive):
		answer("A job is already queued or running")
	case errors.Is(err, store.ErrInvalidTransition):
		if cur, gerr := s.Store.Get(ctx, cb.Key); gerr == nil {
			s.refresh(ctx, cur, msgID)
			answer(fmt.Sprintf("Already %s", cur.State))
		} else {
			answer("Already handled")
		}
	case store.IsNotFound(err):
		answer("Task not found")
	case err != nil:
		s.Log.Error("callback", "data", q.Data, "err", err)
		answer("Error, try again")
	default:
		s.Log.Info("task updated", "key", t.JiraKey, "state", t.State)
		s.refresh(ctx, t, msgID)
		answer(toast)
	}
}

func (s *Service) withLock(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// sleep waits d or until ctx is done; returns false if ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// approvable reports whether a task in state st can still be approved.
func approvable(st store.State) bool {
	return st == store.StateNew || st == store.StateNotified || st == store.StateSnoozed
}

// requeue queues a new job group for an already approved task and returns the task for re-rendering.
func (s *Service) requeue(ctx context.Context, key string, repoNames []string, now time.Time) (store.Task, error) {
	if _, err := s.Store.EnqueueRepos(ctx, key, repoNames, now); err != nil {
		return store.Task{}, err
	}
	return s.Store.Get(ctx, key)
}

// RefreshApproved re-renders every approved notice so its footer/buttons match the latest job
// (e.g. after an upgrade that adds buttons, or a job the worker finished while we were down).
func (s *Service) RefreshApproved(ctx context.Context) {
	tasks, err := s.Store.ListByState(ctx, store.StateApproved)
	if err != nil {
		s.Log.Warn("list approved", "err", err)
		return
	}
	for i, t := range tasks {
		if t.TGMessageID == 0 {
			continue
		}
		if i > 0 && !sleep(ctx, s.SendGap) {
			return
		}
		s.withLock(func() { s.refresh(ctx, t, t.TGMessageID) })
	}
}

// handleHandover applies a handover button; the worker does the GitLab/Jira work.
// Returns the toast text.
func (s *Service) handleHandover(ctx context.Context, cb telegram.Callback, msgID int64) string {
	id, err := strconv.ParseInt(cb.Key, 10, 64)
	if err != nil {
		return "Unknown handover"
	}
	now := s.Now()
	var (
		h      store.Handover
		footer string
		toast  string
	)
	switch cb.Action {
	case telegram.ActHandoverYes:
		h, err = s.Store.DecideHandover(ctx, id, true, time.Time{}, now)
		footer, toast = "\n\n✅ <b>Đã đồng ý</b> "+now.Format("15:04")+" — worker đang tạo MR sang test…", "🚀 OK, đang bàn giao"
	case telegram.ActHandoverLater:
		at := now.Add(s.RemindAfter)
		h, err = s.Store.DecideHandover(ctx, id, false, at, now)
		footer, toast = "\n\n⏰ <b>Để sau</b> — sẽ hỏi lại sau "+at.Format("15:04 02/01"), "⏰ Sẽ hỏi lại sau"
	case telegram.ActHandoverRewatch:
		timeout := s.HandoverTimeout
		if timeout <= 0 {
			timeout = 90 * time.Minute
		}
		if _, err = s.Store.RewatchHandover(ctx, id, now.Add(timeout), now); err == nil {
			if err := s.TG.EditMessage(ctx, s.ChatID, msgID, "🔁 Đang theo dõi lại CI/CD của lần bàn giao #"+cb.Key+"…", nil); err != nil {
				s.Log.Warn("edit handover report", "err", err)
			}
			return "🔁 Đang theo dõi lại"
		}
	}
	switch {
	case errors.Is(err, store.ErrHandoverState):
		return "Đã xử lý rồi"
	case store.IsNotFound(err):
		return "Handover not found"
	case err != nil:
		s.Log.Error("handover callback", "id", id, "err", err)
		return "Error, try again"
	}
	if h.Message != "" {
		if err := s.TG.EditMessage(ctx, s.ChatID, msgID, h.Message+footer, nil); err != nil && !telegram.IsNotModified(err) {
			s.Log.Warn("edit handover question", "err", err)
		}
	}
	s.Log.Info("handover decided", "id", id, "status", h.Status)
	return toast
}
