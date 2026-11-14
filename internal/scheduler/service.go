package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
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

// Service runs sync → notify cycles and handles Telegram button presses.
type Service struct {
	Syncer        *Syncer
	Store         *store.Store
	TG            Messenger
	ChatID        int64
	SnoozeChoices []time.Duration
	Repos         repos.Config // candidate repos per Jira project for "Fix it"
	PollInterval  time.Duration
	RemindAfter   time.Duration // re-remind notified tasks untouched this long
	RemindCheck   time.Duration // how often due reminders are checked (default 1m)
	SendGap       time.Duration // pause between outgoing messages (Telegram: ~1 msg/s per chat)
	Heartbeat     func()        // optional; called each loop tick to prove the main loop is alive
	Now           func() time.Time
	Log           *slog.Logger

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

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.Now()
	var (
		t     store.Task
		toast string
	)
	switch {
	case cb.Action == telegram.ActFix:
		candidates := s.Repos.For(cb.Key)
		switch len(candidates) {
		case 0:
			t, err = s.Store.Approve(ctx, cb.Key, now)
			toast = "✅ Approved (no repo configured)"
		case 1:
			t, _, err = s.Store.ApproveAndEnqueue(ctx, cb.Key, candidates[0].Name, now)
			toast = "🕓 Queued on " + candidates[0].Name
		default:
			// Ask which repo first; nothing changes until a repo is picked.
			if t, err = s.Store.Get(ctx, cb.Key); err == nil {
				if !approvable(t.State) {
					err = store.ErrInvalidTransition
					break
				}
				names := make([]string, len(candidates))
				for i, r := range candidates {
					names[i] = r.Name
				}
				text, _ := s.view(t)
				if err := s.TG.EditMessage(ctx, s.ChatID, msgID, text, telegram.RepoKeyboard(t.JiraKey, names)); err != nil && !telegram.IsNotModified(err) {
					s.Log.Warn("edit message", "key", t.JiraKey, "err", err)
				}
				answer("Choose a repo")
				return
			}
		}
	case cb.Action == telegram.ActRepo:
		candidates := s.Repos.For(cb.Key)
		if cb.RepoIndex >= len(candidates) {
			answer("Unknown repo — repos.json changed?")
			return
		}
		repo := candidates[cb.RepoIndex].Name
		t, _, err = s.Store.ApproveAndEnqueue(ctx, cb.Key, repo, now)
		toast = "🕓 Queued on " + repo
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
