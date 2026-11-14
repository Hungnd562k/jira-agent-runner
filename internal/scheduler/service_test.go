package scheduler

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"
)

type sent struct {
	text    string
	kb      telegram.Keyboard
	replyTo int64
}

type edit struct {
	msgID int64
	text  string
	kb    telegram.Keyboard
}

type fakeTG struct {
	nextID  int64
	sent    []sent
	edits   []edit
	answers []string
}

func (f *fakeTG) SendMessage(_ context.Context, _ int64, html string, kb telegram.Keyboard, replyTo int64) (int64, error) {
	f.nextID++
	f.sent = append(f.sent, sent{html, kb, replyTo})
	return f.nextID, nil
}
func (f *fakeTG) EditMessage(_ context.Context, _, id int64, html string, kb telegram.Keyboard) error {
	f.edits = append(f.edits, edit{id, html, kb})
	return nil
}
func (f *fakeTG) AnswerCallback(_ context.Context, _, text string) error {
	f.answers = append(f.answers, text)
	return nil
}
func (f *fakeTG) GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}

const chatID = 100

func newService(t *testing.T, issues ...jira.Issue) (*Service, *fakeSource, *fakeTG) {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	src := &fakeSource{issues: issues}
	tg := &fakeTG{}
	return &Service{
		Syncer:        &Syncer{Source: src, Store: st, JQL: "x", Now: now},
		Store:         st,
		TG:            tg,
		ChatID:        chatID,
		SnoozeChoices: []time.Duration{30 * time.Minute, 2 * time.Hour},
		Now:           now,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, src, tg
}

func press(s *Service, chat, msgID int64, data string) {
	s.HandleUpdate(context.Background(), telegram.Update{CallbackQuery: &telegram.CallbackQuery{
		ID: "cb", Data: data, Message: &telegram.Message{MessageID: msgID, Chat: telegram.Chat{ID: chat}},
	}})
}

func TestCycleSendsNewOnce(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"}, jira.Issue{Key: "A-2", Summary: "bug2", URL: "u"})

	if err := s.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tg.sent) != 2 || !strings.Contains(tg.sent[0].text, "[A-1]") || len(tg.sent[0].kb[0]) != 2 {
		t.Fatalf("sent = %+v", tg.sent)
	}
	task, _ := s.Store.Get(ctx, "A-1")
	if task.State != store.StateNotified || task.TGMessageID != 1 {
		t.Fatalf("task = %+v", task)
	}

	if err := s.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tg.sent) != 2 {
		t.Fatalf("re-sent on second cycle: %d", len(tg.sent))
	}
}

func TestFixItApproves(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Cycle(ctx)

	press(s, chatID, 1, "fix:A-1")
	task, _ := s.Store.Get(ctx, "A-1")
	if task.State != store.StateApproved {
		t.Fatalf("state = %s", task.State)
	}
	last := tg.edits[len(tg.edits)-1]
	if last.msgID != 1 || !strings.Contains(last.text, "Approved") || len(last.kb) != 0 {
		t.Fatalf("edit = %+v", last)
	}

	press(s, chatID, 1, "fix:A-1")
	if got := tg.answers[len(tg.answers)-1]; got != "Already approved" {
		t.Errorf("double press answer = %q", got)
	}
}

func TestSnoozeFlow(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Cycle(ctx)

	press(s, chatID, 1, "snz:A-1")
	menu := tg.edits[len(tg.edits)-1].kb
	if len(menu) != 2 || len(menu[0]) != 2 || menu[0][0].CallbackData != "snz:A-1:30m0s" {
		t.Fatalf("snooze menu = %+v", menu)
	}

	press(s, chatID, 1, menu[0][1].CallbackData) // 2h
	task, _ := s.Store.Get(ctx, "A-1")
	if task.State != store.StateSnoozed || !task.RemindAt.Equal(s.Now().Add(2*time.Hour)) {
		t.Fatalf("task = %+v", task)
	}
	if !strings.Contains(tg.edits[len(tg.edits)-1].text, "Snoozed until") {
		t.Errorf("edit text = %q", tg.edits[len(tg.edits)-1].text)
	}
}

func TestCallbackFromOtherChatIgnored(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Cycle(ctx)

	press(s, 999, 1, "fix:A-1")
	task, _ := s.Store.Get(ctx, "A-1")
	if task.State != store.StateNotified {
		t.Fatalf("foreign chat changed state to %s", task.State)
	}
	if tg.answers[len(tg.answers)-1] != "Not allowed" {
		t.Errorf("answers = %v", tg.answers)
	}
}

func TestClosedIssueStrikesMessage(t *testing.T) {
	ctx := context.Background()
	s, src, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Cycle(ctx)

	src.issues = nil
	s.Cycle(ctx)
	last := tg.edits[len(tg.edits)-1]
	if last.msgID != 1 || !strings.Contains(last.text, "<s>") || len(last.kb) != 0 {
		t.Fatalf("closed edit = %+v", last)
	}
}

func TestRemindUntouchedAndSnoozed(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"}, jira.Issue{Key: "A-2", Summary: "bug2", URL: "u"})
	clock := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return clock }
	s.Syncer.Now = s.Now
	s.RemindAfter = 4 * time.Hour

	s.Cycle(ctx) // A-1 → msg 1, A-2 → msg 2
	press(s, chatID, 2, "snz:A-2:30m0s")

	clock = clock.Add(29 * time.Minute)
	s.Remind(ctx)
	if len(tg.sent) != 2 {
		t.Fatalf("reminded too early: %d sends", len(tg.sent))
	}

	clock = clock.Add(time.Minute) // snooze expired
	s.Remind(ctx)
	if len(tg.sent) != 3 {
		t.Fatalf("snoozed task not reminded: %d sends", len(tg.sent))
	}
	r := tg.sent[2]
	if r.replyTo != 2 || !strings.Contains(r.text, "Reminder") || len(r.kb) != 1 {
		t.Fatalf("reminder = %+v", r)
	}
	old := tg.edits[len(tg.edits)-1]
	if old.msgID != 2 || len(old.kb) != 0 || !strings.Contains(old.text, "Reminder sent below") {
		t.Fatalf("old message not stripped: %+v", old)
	}
	task, _ := s.Store.Get(ctx, "A-2")
	if task.State != store.StateNotified || task.TGMessageID != 3 || !task.RemindAt.IsZero() {
		t.Fatalf("A-2 after reminder = %+v", task)
	}

	clock = time.Unix(1_800_000_000, 0).Add(4 * time.Hour) // A-1 untouched for RemindAfter
	s.Remind(ctx)
	if len(tg.sent) != 4 || tg.sent[3].replyTo != 1 {
		t.Fatalf("untouched task not reminded: %+v", tg.sent)
	}

	s.Remind(ctx) // nothing due right after reminding
	if len(tg.sent) != 4 {
		t.Fatalf("duplicate reminder: %d sends", len(tg.sent))
	}
}

func TestApprovedNeverReminded(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	clock := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return clock }
	s.Syncer.Now = s.Now
	s.RemindAfter = time.Hour

	s.Cycle(ctx)
	press(s, chatID, 1, "fix:A-1")
	clock = clock.Add(48 * time.Hour)
	s.Remind(ctx)
	if len(tg.sent) != 1 {
		t.Fatalf("approved task reminded: %d sends", len(tg.sent))
	}
}
