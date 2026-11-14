package jira

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
)

// Comment is a Jira issue comment.
type Comment struct {
	Author  string
	Body    string
	Created time.Time
}

// IssueDetail is the full context handed to the coding agent.
type IssueDetail struct {
	Issue
	Type        string
	Description string
	Components  []string
	Labels      []string
	Comments    []Comment // most recent last, capped by GetIssue
}

// maxComments caps how many of the latest comments are kept (prompt size).
const maxComments = 5

type issueResponse struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Updated     string `json:"updated"`
		Labels      []string
		IssueType   *struct{ Name string }  `json:"issuetype"`
		Priority    *struct{ Name string }  `json:"priority"`
		Status      *struct{ Name string }  `json:"status"`
		Components  []struct{ Name string } `json:"components"`
		Comment     *struct {
			Comments []struct {
				Body    string `json:"body"`
				Created string `json:"created"`
				Author  *struct {
					DisplayName string `json:"displayName"`
				} `json:"author"`
			} `json:"comments"`
		} `json:"comment"`
	} `json:"fields"`
}

// GetIssue fetches one issue with description, components and the latest comments.
func (c *Client) GetIssue(ctx context.Context, key string) (IssueDetail, error) {
	q := url.Values{}
	q.Set("fields", "summary,description,issuetype,priority,status,components,labels,comment,updated")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/rest/api/2/issue/"+url.PathEscape(key)+"?"+q.Encode(), nil)
	if err != nil {
		return IssueDetail{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return IssueDetail{}, fmt.Errorf("jira get issue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return IssueDetail{}, fmt.Errorf("jira get issue %s: HTTP %d: %s", key, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var ir issueResponse
	if err := json.NewDecoder(resp.Body).Decode(&ir); err != nil {
		return IssueDetail{}, fmt.Errorf("jira get issue: decode: %w", err)
	}
	f := ir.Fields
	d := IssueDetail{
		Issue:       Issue{Key: ir.Key, Summary: f.Summary, URL: c.BrowseURL(ir.Key), Updated: parseTime(f.Updated)},
		Description: f.Description,
		Labels:      f.Labels,
	}
	if f.IssueType != nil {
		d.Type = f.IssueType.Name
	}
	if f.Priority != nil {
		d.Priority = f.Priority.Name
	}
	if f.Status != nil {
		d.Status = f.Status.Name
	}
	for _, comp := range f.Components {
		d.Components = append(d.Components, comp.Name)
	}
	if f.Comment != nil {
		cs := f.Comment.Comments
		if len(cs) > maxComments {
			cs = cs[len(cs)-maxComments:]
		}
		for _, cm := range cs {
			out := Comment{Body: cm.Body, Created: parseTime(cm.Created)}
			if cm.Author != nil {
				out.Author = cm.Author.DisplayName
			}
			d.Comments = append(d.Comments, out)
		}
	}
	return d, nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(jiraTimeLayout, s)
	return t
}

// AddComment posts a comment (Jira Server wiki markup) on an issue.
func (c *Client) AddComment(ctx context.Context, key, body string) error {
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/rest/api/2/issue/"+url.PathEscape(key)+"/comment", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira add comment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jira add comment %s: HTTP %d: %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
