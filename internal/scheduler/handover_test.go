package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

// proposed creates a merged job for A-1 and a proposed handover with its question message.
func proposed(t *testing.T, s *Service) store.Handover {
	t.Helper()
	ctx := context.Background()
	now := s.Now()
	s.Store.Upsert(ctx, store.IssueInput{Key: "A-1", Summary: "bug", URL: "u"}, now)
	s.Store.MarkNotified(ctx, "A-1", 1, now)
	s.Store.ApproveAndEnqueue(ctx, "A-1", "web", now)
	j, _, _ := s.Store.ClaimJob(ctx, now)
	s.Store.FinishJob(ctx, j.ID, store.JobResult{Status: store.JobSucceeded, MRURL: "mr", MergedSHA: "abc"}, now)
	h, err := s.Store.ProposeHandover(ctx, []int64{j.ID}, []store.HandoverRepo{{RepoName: "web", Project: "g/web"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	s.Store.SetHandoverMessage(ctx, h.ID, 50, "🚀 Bàn giao lên test?")
	return h
}

func TestHandoverYes(t *testing.T) {
	s, _, tg := newService(t, jira.Issue{Key: "A-1", Summary: "bug", URL: "u"})
	h := proposed(t, s)

	press(s, chatID, 50, fmt.Sprintf("hoy:%d", h.ID))
	got, _ := s.Store.GetHandover(context.Background(), h.ID)
	if got.Status != store.HandoverApproved {
		t.Fatalf("status = %s", got.Status)
	}
	e := tg.edits[len(tg.edits)-1]
	if e.msgID != 50 || !strings.Contains(e.text, "Bàn giao lên test?") || !strings.Contains(e.text, "Đã đồng ý") || e.kb != nil {
		t.Fatalf("edit = %+v", e)
	}
	press(s, chatID, 50, fmt.Sprintf("hoy:%d", h.ID))
	if tg.answers[len(tg.answers)-1] != "Đã xử lý rồi" {
		t.Errorf("double click answer = %q", tg.answers[len(tg.answers)-1])
	}
}

func TestHandoverLater(t *testing.T) {
	s, _, tg := newService(t)
	s.RemindAfter = 4 * time.Hour
	h := proposed(t, s)

	press(s, chatID, 50, fmt.Sprintf("hol:%d", h.ID))
	got, _ := s.Store.GetHandover(context.Background(), h.ID)
	if got.Status != store.HandoverSnoozed || !got.RemindAt.Equal(s.Now().Add(4*time.Hour)) || len(got.Jobs) != 0 {
		t.Fatalf("handover = %+v", got)
	}
	if !strings.Contains(tg.edits[len(tg.edits)-1].text, "Để sau") {
		t.Errorf("edit = %+v", tg.edits)
	}
}

func TestHandoverFromOtherChatIgnored(t *testing.T) {
	s, _, _ := newService(t)
	h := proposed(t, s)
	press(s, 999, 50, fmt.Sprintf("hoy:%d", h.ID))
	if got, _ := s.Store.GetHandover(context.Background(), h.ID); got.Status != store.HandoverProposed {
		t.Fatalf("status = %s", got.Status)
	}
}
