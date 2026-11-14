// Package jira is a minimal Jira Server/Data Center REST API v2 client.
package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Issue is the subset of a Jira issue the runner cares about.
type Issue struct {
	Key      string
	Summary  string
	Priority string
	Status   string
	URL      string
	Updated  time.Time
}

// Client talks to Jira using a Personal Access Token (Bearer auth).
type Client struct {
	baseURL  string
	pat      string
	http     *http.Client
	pageSize int
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client (default: 30s timeout).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithPageSize sets maxResults per search request (default 100).
func WithPageSize(n int) Option { return func(c *Client) { c.pageSize = n } }

// New creates a client for the given base URL (e.g. https://jira.example.com).
func New(baseURL, pat string, opts ...Option) *Client {
	c := &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		pat:      pat,
		http:     &http.Client{Timeout: 30 * time.Second},
		pageSize: 100,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// BrowseURL returns the human-facing link for an issue key.
func (c *Client) BrowseURL(key string) string { return c.baseURL + "/browse/" + key }

// jiraTimeLayout is the format Jira Server uses, e.g. 2026-11-10T10:00:00.000+0700.
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

type searchResponse struct {
	StartAt    int `json:"startAt"`
	MaxResults int `json:"maxResults"`
	Total      int `json:"total"`
	Issues     []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary  string `json:"summary"`
			Updated  string `json:"updated"`
			Priority *struct {
				Name string `json:"name"`
			} `json:"priority"`
			Status *struct {
				Name string `json:"name"`
			} `json:"status"`
		} `json:"fields"`
	} `json:"issues"`
}

// Search runs a JQL query and returns all matching issues, following pagination.
func (c *Client) Search(ctx context.Context, jql string) ([]Issue, error) {
	var out []Issue
	startAt := 0
	for {
		page, err := c.searchPage(ctx, jql, startAt)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Issues {
			iss := Issue{
				Key:     it.Key,
				Summary: it.Fields.Summary,
				URL:     c.BrowseURL(it.Key),
			}
			if it.Fields.Priority != nil {
				iss.Priority = it.Fields.Priority.Name
			}
			if it.Fields.Status != nil {
				iss.Status = it.Fields.Status.Name
			}
			if it.Fields.Updated != "" {
				if t, err := time.Parse(jiraTimeLayout, it.Fields.Updated); err == nil {
					iss.Updated = t
				}
			}
			out = append(out, iss)
		}
		startAt += len(page.Issues)
		if len(page.Issues) == 0 || startAt >= page.Total {
			return out, nil
		}
	}
}

func (c *Client) searchPage(ctx context.Context, jql string, startAt int) (*searchResponse, error) {
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("startAt", strconv.Itoa(startAt))
	q.Set("maxResults", strconv.Itoa(c.pageSize))
	q.Set("fields", "summary,priority,status,updated")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/rest/api/2/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("jira search: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("jira search: decode: %w", err)
	}
	return &sr, nil
}
