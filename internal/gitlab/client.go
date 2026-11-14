// Package gitlab is a minimal GitLab REST v4 client for opening draft merge requests.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/worker"
)

// Client calls a (self-hosted) GitLab instance with a token that has `api` scope.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New creates a client for baseURL (e.g. https://gitlab.example.com).
func New(baseURL, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

type mrCreate struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	RemoveSourceBranch bool   `json:"remove_source_branch"`
}

// OpenDraftMR creates the MR and returns its web URL. The "Draft:" title prefix marks it draft.
func (c *Client) OpenDraftMR(ctx context.Context, r worker.MRRequest) (string, error) {
	title := r.Title
	if !strings.HasPrefix(title, "Draft:") {
		title = "Draft: " + title
	}
	body, err := json.Marshal(mrCreate{
		SourceBranch:       r.SourceBranch,
		TargetBranch:       r.Repo.BaseBranch,
		Title:              title,
		Description:        r.Description,
		RemoveSourceBranch: true,
	})
	if err != nil {
		return "", err
	}
	endpoint := c.baseURL + "/api/v4/projects/" + url.PathEscape(r.Repo.GitLabProject) + "/merge_requests"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gitlab create MR: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("gitlab create MR: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		WebURL string `json:"web_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("gitlab create MR: decode: %w", err)
	}
	return out.WebURL, nil
}
