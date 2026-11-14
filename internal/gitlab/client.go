// Package gitlab is a minimal GitLab REST v4 client: merge requests, approvals, merges and
// CI/CD pipeline status for the agent worker.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/worker"
)

// Client calls a (self-hosted) GitLab instance with a token that has `api` scope.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// mergeWait is the pause between mergeability checks (GitLab computes it asynchronously).
	mergeWait time.Duration
}

// New creates a client for baseURL (e.g. https://gitlab.example.com).
func New(baseURL, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token,
		http: &http.Client{Timeout: 30 * time.Second}, mergeWait: 3 * time.Second}
}

// APIError is a non-2xx GitLab response.
type APIError struct {
	Op     string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab %s: HTTP %d: %s", e.Op, e.Status, e.Body)
}

func projectPath(p string) string { return "/api/v4/projects/" + url.PathEscape(p) }

// do sends a JSON request; out may be nil. Only 2xx is success.
func (c *Client) do(ctx context.Context, op, method, path string, in, out any) error {
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
	req.Header.Set("PRIVATE-TOKEN", c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Op: op, Status: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		*s = string(b)
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gitlab %s: decode: %w", op, err)
	}
	return nil
}

type mrCreate struct {
	SourceBranch       string `json:"source_branch"`
	TargetBranch       string `json:"target_branch"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	RemoveSourceBranch bool   `json:"remove_source_branch"`
}

type mrJSON struct {
	IID                 int64  `json:"iid"`
	WebURL              string `json:"web_url"`
	State               string `json:"state"`
	MergeCommitSHA      string `json:"merge_commit_sha"`
	SquashCommitSHA     string `json:"squash_commit_sha"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	MergeStatus         string `json:"merge_status"`
	HasConflicts        bool   `json:"has_conflicts"`
}

func (m mrJSON) toMR() worker.MR { return worker.MR{IID: m.IID, WebURL: m.WebURL} }

// OpenMR creates a merge request (draft when r.Draft) targeting r.Target or the repo's base branch.
func (c *Client) OpenMR(ctx context.Context, r worker.MRRequest) (worker.MR, error) {
	title := r.Title
	if r.Draft && !strings.HasPrefix(title, "Draft:") {
		title = "Draft: " + title
	}
	target := r.Target
	if target == "" {
		target = r.Repo.BaseBranch
	}
	var out mrJSON
	err := c.do(ctx, "create MR", http.MethodPost, projectPath(r.Repo.GitLabProject)+"/merge_requests", mrCreate{
		SourceBranch:       r.SourceBranch,
		TargetBranch:       target,
		Title:              title,
		Description:        r.Description,
		RemoveSourceBranch: !r.KeepSource,
	}, &out)
	return out.toMR(), err
}

// FindOpenMR returns an open MR from source to target, if any.
func (c *Client) FindOpenMR(ctx context.Context, project, source, target string) (worker.MR, bool, error) {
	q := url.Values{"state": {"opened"}, "source_branch": {source}, "target_branch": {target}}
	var out []mrJSON
	if err := c.do(ctx, "find MR", http.MethodGet, projectPath(project)+"/merge_requests?"+q.Encode(), nil, &out); err != nil {
		return worker.MR{}, false, err
	}
	if len(out) == 0 {
		return worker.MR{}, false, nil
	}
	return out[0].toMR(), true, nil
}

// ApproveMR approves as the token's user. GitLab answers 401 when the user already approved.
func (c *Client) ApproveMR(ctx context.Context, project string, iid int64) error {
	return c.do(ctx, "approve MR", http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/approve", projectPath(project), iid), nil, nil)
}

// MergeMR waits until GitLab has computed mergeability, merges and returns the merge commit SHA.
// An MR that is already merged returns its existing merge commit.
func (c *Client) MergeMR(ctx context.Context, project string, iid int64, removeSource bool) (string, error) {
	path := fmt.Sprintf("%s/merge_requests/%d", projectPath(project), iid)
	var mr mrJSON
	for i := 0; ; i++ {
		if err := c.do(ctx, "get MR", http.MethodGet, path, nil, &mr); err != nil {
			return "", err
		}
		if mr.State == "merged" {
			return mergeSHA(mr), nil
		}
		checking := mr.DetailedMergeStatus == "checking" || mr.DetailedMergeStatus == "unchecked" ||
			mr.DetailedMergeStatus == "preparing" || mr.MergeStatus == "checking" || mr.MergeStatus == "unchecked"
		if !checking || i >= 20 {
			break
		}
		if err := sleep(ctx, c.mergeWait); err != nil {
			return "", err
		}
	}
	if mr.HasConflicts {
		return "", fmt.Errorf("gitlab merge MR !%d: conflicts with the target branch", iid)
	}
	var out mrJSON
	err := c.do(ctx, "merge MR", http.MethodPut, path+"/merge", map[string]any{"should_remove_source_branch": removeSource}, &out)
	if err != nil {
		if mr.DetailedMergeStatus != "" && mr.DetailedMergeStatus != "mergeable" {
			return "", fmt.Errorf("%w (merge status: %s)", err, mr.DetailedMergeStatus)
		}
		return "", err
	}
	if sha := mergeSHA(out); sha != "" {
		return sha, nil
	}
	return "", fmt.Errorf("gitlab merge MR !%d: no merge commit in response (state %q)", iid, out.State)
}

func mergeSHA(m mrJSON) string {
	if m.MergeCommitSHA != "" {
		return m.MergeCommitSHA
	}
	return m.SquashCommitSHA // fast-forward + squash projects
}

type pipelineJSON struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	WebURL string `json:"web_url"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
}

func (p pipelineJSON) toPipeline() worker.Pipeline {
	return worker.Pipeline{ID: p.ID, Status: p.Status, WebURL: p.WebURL}
}

// FindPipeline returns the newest pipeline for ref (at sha when set), if one exists yet.
func (c *Client) FindPipeline(ctx context.Context, project, ref, sha string) (worker.Pipeline, bool, error) {
	q := url.Values{"ref": {ref}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"1"}}
	if sha != "" { // empty = newest pipeline on ref
		q.Set("sha", sha)
	}
	var out []pipelineJSON
	if err := c.do(ctx, "list pipelines", http.MethodGet, projectPath(project)+"/pipelines?"+q.Encode(), nil, &out); err != nil {
		return worker.Pipeline{}, false, err
	}
	if len(out) == 0 {
		return worker.Pipeline{}, false, nil
	}
	return out[0].toPipeline(), true, nil
}

// GetPipeline returns a pipeline's current status.
func (c *Client) GetPipeline(ctx context.Context, project string, id int64) (worker.Pipeline, error) {
	var out pipelineJSON
	err := c.do(ctx, "get pipeline", http.MethodGet, fmt.Sprintf("%s/pipelines/%d", projectPath(project), id), nil, &out)
	return out.toPipeline(), err
}

// FailedJobs lists the failed jobs of a pipeline.
func (c *Client) FailedJobs(ctx context.Context, project string, pipelineID int64) ([]worker.PipelineJob, error) {
	var out []struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		Stage         string `json:"stage"`
		WebURL        string `json:"web_url"`
		FailureReason string `json:"failure_reason"`
	}
	path := fmt.Sprintf("%s/pipelines/%d/jobs?scope[]=failed&per_page=50", projectPath(project), pipelineID)
	if err := c.do(ctx, "pipeline jobs", http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	jobs := make([]worker.PipelineJob, 0, len(out))
	for _, j := range out {
		jobs = append(jobs, worker.PipelineJob{ID: j.ID, Name: j.Name, Stage: j.Stage, WebURL: j.WebURL, FailureReason: j.FailureReason})
	}
	return jobs, nil
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|\r`)

// JobLogTail returns the last n lines of a job's log, without ANSI colors or GitLab section markers.
func (c *Client) JobLogTail(ctx context.Context, project string, jobID int64, n int) (string, error) {
	var log string
	if err := c.do(ctx, "job log", http.MethodGet, projectPath(project)+"/jobs/"+strconv.FormatInt(jobID, 10)+"/trace", nil, &log); err != nil {
		return "", err
	}
	var lines []string
	for _, l := range strings.Split(ansi.ReplaceAllString(log, ""), "\n") {
		if strings.HasPrefix(l, "section_") || strings.TrimSpace(l) == "" {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
