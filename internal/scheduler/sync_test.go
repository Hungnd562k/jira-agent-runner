package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

type fakeSource struct {
	issues []jira.Issue
	err    error
}

func (f *fakeSource) Search(context.Context, string) ([]jira.Issue, error) { return f.issues, f.err }

func TestSyncCreatesAndCloses(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	src := &fakeSource{issues: []jira.Issue{{Key: "A-1", Summary: "s", URL: "u"}, {Key: "A-2", Summary: "s", URL: "u"}}}
	s := &Syncer{Source: src, Store: st, JQL: "x", Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}

	res, err := s.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 2 || len(res.Closed) != 0 {
		t.Fatalf("first sync: %+v", res)
	}

	src.issues = src.issues[:1]
	if res, err = s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 0 || len(res.Closed) != 1 || res.Closed[0].JiraKey != "A-2" {
		t.Fatalf("second sync: %+v", res)
	}
}

func TestSyncJiraErrorClosesNothing(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, ":memory:")
	defer st.Close()

	src := &fakeSource{issues: []jira.Issue{{Key: "A-1", Summary: "s", URL: "u"}}}
	s := &Syncer{Source: src, Store: st, JQL: "x", Now: time.Now}
	if _, err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	src.issues, src.err = nil, errors.New("boom")
	if _, err := s.Sync(ctx); err == nil {
		t.Fatal("expected error")
	}
	if task, _ := st.Get(ctx, "A-1"); task.State != store.StateNew {
		t.Errorf("task closed despite Jira error: %s", task.State)
	}
}
