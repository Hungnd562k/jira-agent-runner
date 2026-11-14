// Package repos loads the Jira project → GitLab repositories mapping (repos.json).
package repos

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Repo is one candidate repository for fixing issues of a Jira project.
type Repo struct {
	Name          string   `json:"name"`           // short label shown on Telegram buttons
	GitLabProject string   `json:"gitlab_project"` // path with namespace, e.g. group/sub/api
	Path          string   `json:"path"`           // absolute path of an existing local clone on the worker host
	BaseBranch    string   `json:"base_branch"`    // branch to fork from and target of the MR
	TestCmd       string   `json:"test_cmd"`       // shell command run in the worktree for test evidence
	AllowedTools  []string `json:"allowed_tools"`  // extra Claude Code tool permissions, e.g. "Bash(dotnet test:*)"
}

// Config maps a Jira project key (e.g. "HDBCD") to its candidate repos.
type Config map[string][]Repo

// Load reads and validates path. A missing file yields an empty config (no repos configured).
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, list := range c {
		for i := range list {
			list[i].GitLabProject = NormalizeProject(list[i].GitLabProject)
		}
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// NormalizeProject turns a clone URL into the GitLab project path the API expects:
// "git@host:group/app.git", "ssh://git@host:22/group/app.git" and
// "https://host/group/app.git" all become "group/app". A plain path is kept.
func NormalizeProject(p string) string {
	p = strings.TrimSpace(p)
	switch {
	case strings.Contains(p, "://"):
		_, rest, _ := strings.Cut(p, "://")
		if _, path, ok := strings.Cut(rest, "/"); ok {
			p = path
		}
	case strings.HasPrefix(p, "git@") || (strings.Contains(p, "@") && strings.Contains(p, ":")):
		if _, path, ok := strings.Cut(p, ":"); ok {
			p = path
		}
	}
	return strings.Trim(strings.TrimSuffix(p, ".git"), "/")
}

func (c Config) validate() error {
	var errs []error
	for project, list := range c {
		seen := map[string]bool{}
		for i, r := range list {
			where := fmt.Sprintf("%s[%d]", project, i)
			switch {
			case r.Name == "":
				errs = append(errs, fmt.Errorf("%s: name is required", where))
			case seen[r.Name]:
				errs = append(errs, fmt.Errorf("%s: duplicate name %q", where, r.Name))
			case strings.Contains(r.Name, ":"):
				errs = append(errs, fmt.Errorf("%s: name must not contain ':'", where))
			}
			seen[r.Name] = true
			if r.GitLabProject == "" {
				errs = append(errs, fmt.Errorf("%s: gitlab_project is required", where))
			}
			if !filepath.IsAbs(r.Path) {
				errs = append(errs, fmt.Errorf("%s: path must be absolute", where))
			}
			if r.BaseBranch == "" {
				errs = append(errs, fmt.Errorf("%s: base_branch is required", where))
			}
		}
	}
	return errors.Join(errs...)
}

// ProjectOf returns the project part of an issue key ("HDBCD-123" → "HDBCD").
func ProjectOf(issueKey string) string {
	if i := strings.LastIndex(issueKey, "-"); i > 0 {
		return issueKey[:i]
	}
	return issueKey
}

// For returns the candidate repos for an issue key.
func (c Config) For(issueKey string) []Repo { return c[ProjectOf(issueKey)] }

// Find returns the repo named name for an issue key.
func (c Config) Find(issueKey, name string) (Repo, bool) {
	for _, r := range c.For(issueKey) {
		if r.Name == name {
			return r, true
		}
	}
	return Repo{}, false
}
