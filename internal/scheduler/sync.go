// Package scheduler drives the poll/remind cycle.
package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

// IssueSource fetches issues from Jira.
type IssueSource interface {
	Search(ctx context.Context, jql string) ([]jira.Issue, error)
}

// TaskStore is the subset of store.Store the syncer needs.
type TaskStore interface {
	Upsert(ctx context.Context, in store.IssueInput, now time.Time) (store.UpsertResult, error)
	CloseMissing(ctx context.Context, seen []string, now time.Time) ([]store.Task, error)
}

// SyncResult summarizes one Jira → store sync.
type SyncResult struct {
	Issues   []jira.Issue
	Created  []string
	Updated  []string
	Reopened []string
	Closed   []store.Task
}

// Syncer mirrors the JQL result set into the store.
type Syncer struct {
	Source IssueSource
	Store  TaskStore
	JQL    string
	Now    func() time.Time
}

// Sync fetches issues and upserts them; tasks no longer matching the JQL are closed.
// Nothing is closed if the Jira request fails.
func (s *Syncer) Sync(ctx context.Context) (*SyncResult, error) {
	issues, err := s.Source.Search(ctx, s.JQL)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	res := &SyncResult{Issues: issues}
	seen := make([]string, 0, len(issues))
	for _, iss := range issues {
		seen = append(seen, iss.Key)
		r, err := s.Store.Upsert(ctx, store.IssueInput{
			Key: iss.Key, Summary: iss.Summary, Priority: iss.Priority,
			Status: iss.Status, URL: iss.URL, Updated: iss.Updated,
		}, now)
		if err != nil {
			return nil, fmt.Errorf("upsert %s: %w", iss.Key, err)
		}
		switch r {
		case store.Created:
			res.Created = append(res.Created, iss.Key)
		case store.Updated:
			res.Updated = append(res.Updated, iss.Key)
		case store.Reopened:
			res.Reopened = append(res.Reopened, iss.Key)
		}
	}
	if res.Closed, err = s.Store.CloseMissing(ctx, seen, now); err != nil {
		return nil, fmt.Errorf("close missing: %w", err)
	}
	return res, nil
}
