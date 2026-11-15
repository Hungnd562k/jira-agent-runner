package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/scope"
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
		Issues:        &fakeIssues{},
		Scope:         scope.Default(),
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

// fakeIssues serves issue comments for the scope comment lookup.
type fakeIssues struct {
	comments map[string][]jira.Comment
	err      error
	calls    int
}

func (f *fakeIssues) GetIssue(_ context.Context, key string) (jira.IssueDetail, error) {
	f.calls++
	if f.err != nil {
		return jira.IssueDetail{}, f.err
	}
	return jira.IssueDetail{Issue: jira.Issue{Key: key, URL: "https://jira/browse/" + key}, Comments: f.comments[key]}, nil
}

func (f *fakeIssues) setScope(key, body string) {
	if f.comments == nil {
		f.comments = map[string][]jira.Comment{}
	}
	f.comments[key] = append(f.comments[key], jira.Comment{Author: "Hung", Body: body})
}

func issuesOf(s *Service) *fakeIssues { return s.Issues.(*fakeIssues) }

func TestFixItWithoutScopeSendsGuide(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Repos = repos.Config{"A": {{Name: "api"}, {Name: "web"}}}
	s.Cycle(ctx)
	issuesOf(s).setScope("A-1", "please fix asap")

	press(s, chatID, 1, "fix:A-1")
	if task, _ := s.Store.Get(ctx, "A-1"); task.State != store.StateNotified {
		t.Fatalf("approved without a scope comment: %s", task.State)
	}
	if _, err := s.Store.LatestJob(ctx, "A-1"); !store.IsNotFound(err) {
		t.Fatalf("job queued without scope: %v", err)
	}
	guide := tg.sent[len(tg.sent)-1]
	for _, want := range []string{"cần phạm vi sửa từ maintainer", "chưa có comment", "<pre>@CLAUDE/CODEX,\nrepo_dependencies: api, web\nspecs: &lt;",
		"hints: ", "repo_dependencies (bắt buộc)", "https://jira/browse/A-1"} {
		if !strings.Contains(guide.text, want) {
			t.Errorf("guide misses %q:\n%s", want, guide.text)
		}
	}
	if guide.replyTo != 1 {
		t.Errorf("guide should reply to the notice, replyTo=%d", guide.replyTo)
	}
	if got := tg.answers[len(tg.answers)-1]; !strings.Contains(got, "Cần phạm vi") {
		t.Errorf("toast = %q", got)
	}

	// An invalid scope comment explains why.
	issuesOf(s).setScope("A-1", "@CLAUDE/CODEX,\nrepo_dependencies: api, billing")
	press(s, chatID, 1, "fix:A-1")
	if g := tg.sent[len(tg.sent)-1].text; !strings.Contains(g, "chưa hợp lệ") || !strings.Contains(g, "billing") {
		t.Errorf("invalid-scope guide = %s", g)
	}

	// Jira down: nothing changes, the user is told to retry.
	issuesOf(s).err = errors.New("jira down")
	press(s, chatID, 1, "fix:A-1")
	if got := tg.answers[len(tg.answers)-1]; !strings.Contains(got, "Không đọc được ticket") {
		t.Errorf("jira error toast = %q", got)
	}
}

func TestFixItQueuesScopedRepos(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Repos = repos.Config{"A": {{Name: "api"}, {Name: "web"}, {Name: "batch"}}}
	s.Cycle(ctx)
	issuesOf(s).setScope("A-1", "---\n@CLAUDE/CODEX,\nrepo_dependencies: web, api\nspecs: thêm cột\nhints: xem Foo\n---")

	press(s, chatID, 1, "fix:A-1")
	jobs, err := s.Store.LatestGroup(ctx, "A-1")
	if err != nil || len(jobs) != 2 || jobs[0].RepoName != "web" || jobs[1].RepoName != "api" || jobs[0].Status != store.JobQueued {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	last := tg.edits[len(tg.edits)-1]
	if !strings.Contains(last.text, "Queued") || !strings.Contains(last.text, "web, api") || len(last.kb) != 0 {
		t.Fatalf("edit = %+v", last)
	}
	if got := tg.answers[len(tg.answers)-1]; got != "🕓 Queued on web, api" {
		t.Errorf("toast = %q", got)
	}
	press(s, chatID, 1, "fix:A-1") // stale button after approval
	if g, _ := s.Store.LatestGroup(ctx, "A-1"); g[0].ID != jobs[0].ID {
		t.Fatal("second group enqueued from stale button")
	}
}

func TestFixItWithoutConfiguredReposApprovesWithoutJira(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Cycle(ctx)
	press(s, chatID, 1, "fix:A-1")
	if task, _ := s.Store.Get(ctx, "A-1"); task.State != store.StateApproved || issuesOf(s).calls != 0 {
		t.Fatalf("state = %s, jira calls = %d", task.State, issuesOf(s).calls)
	}
}

func TestRetryRereadsScopeAndSkipsMerged(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Repos = repos.Config{"A": {{Name: "api"}, {Name: "web"}, {Name: "batch"}}}
	s.Cycle(ctx)
	issuesOf(s).setScope("A-1", "@CLAUDE/CODEX\nrepo_dependencies: api, web")
	press(s, chatID, 1, "fix:A-1")

	press(s, chatID, 1, "rty:A-1") // still queued → refused
	if got := tg.answers[len(tg.answers)-1]; got != "A job is already queued or running" {
		t.Fatalf("retry while queued toast = %q", got)
	}

	group, _, _ := s.Store.ClaimGroup(ctx, s.Now())
	s.Store.FinishJob(ctx, group[0].ID, store.JobResult{Status: store.JobSucceeded, MRURL: "mr", MergedSHA: "abc"}, s.Now())
	s.Store.FinishJob(ctx, group[1].ID, store.JobResult{Status: store.JobFailed, Error: "boom"}, s.Now())

	task, _ := s.Store.Get(ctx, "A-1")
	text, kb := s.render(ctx, task)
	if len(kb) != 1 || len(kb[0]) != 1 || kb[0][0].CallbackData != "rty:A-1" {
		t.Fatalf("finished-group keyboard = %+v", kb)
	}
	if !strings.Contains(text, "Merged vào dev") || !strings.Contains(text, "Agent failed</b> on <code>web") {
		t.Errorf("group footer = %s", text)
	}

	// The maintainer widened the scope: retry re-reads it and skips the merged repo.
	issuesOf(s).setScope("A-1", "@CLAUDE/CODEX\nrepo_dependencies: api, web, batch\nhints: batch job too")
	press(s, chatID, 1, "rty:A-1")
	retry, _ := s.Store.LatestGroup(ctx, "A-1")
	if len(retry) != 2 || retry[0].RepoName != "web" || retry[1].RepoName != "batch" || retry[0].GroupID == group[0].GroupID {
		t.Fatalf("retry group = %+v", retry)
	}
	if got := tg.answers[len(tg.answers)-1]; got != "🔁 Retrying on web, batch" {
		t.Errorf("retry toast = %q", got)
	}

	// Scope comment removed (invalid now): retry sends the guide instead.
	group, _, _ = s.Store.ClaimGroup(ctx, s.Now())
	for _, j := range group {
		s.Store.FinishJob(ctx, j.ID, store.JobResult{Status: store.JobFailed, Error: "x"}, s.Now())
	}
	issuesOf(s).setScope("A-1", "@CLAUDE/CODEX\nspecs: only specs")
	sends := len(tg.sent)
	press(s, chatID, 1, "rty:A-1")
	if len(tg.sent) != sends+1 || !strings.Contains(tg.sent[len(tg.sent)-1].text, "thiếu repo_dependencies") {
		t.Fatalf("expected the scope guide, sent = %+v", tg.sent[sends:])
	}
}

func TestRefreshApprovedAddsJobButtons(t *testing.T) {
	ctx := context.Background()
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	s.Repos = repos.Config{"A": {{Name: "api"}}}
	s.Cycle(ctx)
	issuesOf(s).setScope("A-1", "@CLAUDE/CODEX\nrepo_dependencies: api")
	press(s, chatID, 1, "fix:A-1")
	j, _ := s.Store.LatestJob(ctx, "A-1")
	s.Store.ClaimJob(ctx, s.Now())
	s.Store.FinishJob(ctx, j.ID, store.JobResult{Status: store.JobSucceeded}, s.Now()) // finished by the worker

	s.RefreshApproved(ctx)
	last := tg.edits[len(tg.edits)-1]
	if last.msgID != 1 || len(last.kb) != 1 || last.kb[0][0].CallbackData != "rty:A-1" {
		t.Fatalf("refresh edit = %+v", last)
	}
}
