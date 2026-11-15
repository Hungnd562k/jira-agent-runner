package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Comment is a Jira issue comment.
type Comment struct {
	Author     string // display name
	AuthorName string // Jira username
	Body       string
	Created    time.Time
}

// IssueDetail is the full context handed to the coding agent.
type IssueDetail struct {
	Issue
	Type        string
	Description string
	Components  []string
	Labels      []string
	Comments    []Comment // all comments, most recent last (the prompt keeps only the latest few)
}

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
					Name        string `json:"name"`
					DisplayName string `json:"displayName"`
				} `json:"author"`
			} `json:"comments"`
		} `json:"comment"`
	} `json:"fields"`
}

// GetIssue fetches one issue with description, components and its comments.
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
		for _, cm := range f.Comment.Comments {
			out := Comment{Body: cm.Body, Created: parseTime(cm.Created)}
			if cm.Author != nil {
				out.Author, out.AuthorName = cm.Author.DisplayName, cm.Author.Name
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

// AddAttachment uploads a local file to an issue as name and returns the stored file name
// (use it in wiki markup, e.g. !name|thumbnail!).
func (c *Client) AddAttachment(ctx context.Context, key, path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if name == "" {
		name = filepath.Base(path)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/rest/api/2/issue/"+url.PathEscape(key)+"/attachments", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Atlassian-Token", "no-check")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("jira add attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("jira add attachment %s: HTTP %d: %s", key, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out []struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out) == 0 {
		return name, nil
	}
	return out[0].Filename, nil
}

// jsonDo sends a JSON request with the PAT; out may be nil. want is the success status.
func (c *Client) jsonDo(ctx context.Context, op, method, path string, in, out any, want int) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jira %s: HTTP %d: %s", op, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Transition moves an issue through the workflow transition whose target status (or name)
// equals toStatus, case-insensitive. resolution is sent only when the transition screen has
// a resolution field. Returns the status reached. Already in toStatus = no-op.
func (c *Client) Transition(ctx context.Context, key, toStatus, resolution string) (string, error) {
	base := "/rest/api/2/issue/" + url.PathEscape(key)
	var cur struct {
		Fields struct {
			Status struct{ Name string } `json:"status"`
		} `json:"fields"`
	}
	if err := c.jsonDo(ctx, "get status", http.MethodGet, base+"?fields=status", nil, &cur, http.StatusOK); err != nil {
		return "", err
	}
	if strings.EqualFold(cur.Fields.Status.Name, toStatus) {
		return cur.Fields.Status.Name, nil
	}
	var list struct {
		Transitions []struct {
			ID     string                     `json:"id"`
			Name   string                     `json:"name"`
			To     struct{ Name string }      `json:"to"`
			Fields map[string]json.RawMessage `json:"fields"`
		} `json:"transitions"`
	}
	if err := c.jsonDo(ctx, "transitions", http.MethodGet, base+"/transitions?expand=transitions.fields", nil, &list, http.StatusOK); err != nil {
		return "", err
	}
	var names []string
	for _, t := range list.Transitions {
		names = append(names, fmt.Sprintf("%s→%s", t.Name, t.To.Name))
		if !strings.EqualFold(t.To.Name, toStatus) && !strings.EqualFold(t.Name, toStatus) {
			continue
		}
		payload := map[string]any{"transition": map[string]string{"id": t.ID}}
		if _, ok := t.Fields["resolution"]; ok && resolution != "" {
			payload["fields"] = map[string]any{"resolution": map[string]string{"name": resolution}}
		}
		if err := c.jsonDo(ctx, "transition", http.MethodPost, base+"/transitions", payload, nil, http.StatusNoContent); err != nil {
			return "", err
		}
		return t.To.Name, nil
	}
	return "", fmt.Errorf("jira transition %s: no transition to %q from %q (available: %s)",
		key, toStatus, cur.Fields.Status.Name, strings.Join(names, ", "))
}

// AssignToReporter assigns the issue to its reporter and returns the reporter's display name.
func (c *Client) AssignToReporter(ctx context.Context, key string) (string, error) {
	base := "/rest/api/2/issue/" + url.PathEscape(key)
	var is struct {
		Fields struct {
			Reporter *struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"reporter"`
		} `json:"fields"`
	}
	if err := c.jsonDo(ctx, "get reporter", http.MethodGet, base+"?fields=reporter", nil, &is, http.StatusOK); err != nil {
		return "", err
	}
	r := is.Fields.Reporter
	if r == nil || r.Name == "" {
		return "", fmt.Errorf("jira assign %s: issue has no reporter", key)
	}
	if err := c.jsonDo(ctx, "assign", http.MethodPut, base+"/assignee", map[string]string{"name": r.Name}, nil, http.StatusNoContent); err != nil {
		return "", err
	}
	return r.DisplayName, nil
}
