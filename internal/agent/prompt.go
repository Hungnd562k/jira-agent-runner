package agent

import (
	"fmt"
	"strings"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
)

// Task is the per-job context added to the prompt next to the issue.
type Task struct {
	// Repos the agent may change; the first one is the current directory. Each is a fresh git
	// worktree on its own branch.
	Repos []RepoTask
	// Scope is the maintainer's scope comment (specs, hints…); empty when there is none.
	Scope       []ScopeNote
	ScopeAuthor string
}

// RepoTask is one repository in the job's scope.
type RepoTask struct {
	Name    string
	Dir     string // absolute worktree path
	TestCmd string
	E2E     *E2E // nil = no browser verification for this repo
	// ResumeOf is the interrupted job whose partial changes are already in this worktree (0 = fresh).
	ResumeOf int64
}

// ScopeNote is one guidance field of the maintainer's scope comment.
type ScopeNote struct {
	Key, Description, Value string
}

// E2E describes the local web app the agent must verify in a browser.
type E2E struct {
	BaseURL     string
	StartCmd    string
	Profile     string // credentials profile name
	Notes       string // repo-specific hints (login flow, menus, test data)
	EvidenceDir string // absolute, outside the worktree
}

// HasE2E reports whether any repo of the task needs browser verification.
func (t Task) HasE2E() bool {
	for _, r := range t.Repos {
		if r.E2E != nil {
			return true
		}
	}
	return false
}

// promptComments is how many of the latest issue comments go into the prompt.
const promptComments = 5

// BuildPrompt turns a Jira issue into the task for the coding agent.
// The standing rules (Rules) go into the system prompt; this is the job itself.
func BuildPrompt(d jira.IssueDetail, t Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fix Jira issue %s in the repositories listed under \"Repositories\" (the first is the current directory; each is a fresh git branch). Follow the rules in your system prompt, including the scope rules.\n\n", d.Key)
	fmt.Fprintf(&b, "## Issue\n- Key: %s\n- Type: %s\n- Priority: %s\n- Status: %s\n", d.Key, d.Type, d.Priority, d.Status)
	if len(d.Components) > 0 {
		fmt.Fprintf(&b, "- Components: %s\n", strings.Join(d.Components, ", "))
	}
	if len(d.Labels) > 0 {
		fmt.Fprintf(&b, "- Labels: %s\n", strings.Join(d.Labels, ", "))
	}
	fmt.Fprintf(&b, "- Summary: %s\n\n### Description\n%s\n", d.Summary, orNone(d.Description))
	if cs := d.Comments; len(cs) > 0 {
		if len(cs) > promptComments {
			cs = cs[len(cs)-promptComments:]
		}
		b.WriteString("\n### Latest comments\n")
		for _, c := range cs {
			fmt.Fprintf(&b, "- %s (%s): %s\n", c.Author, c.Created.Format("2006-01-02"), strings.TrimSpace(c.Body))
		}
	}

	names := make([]string, len(t.Repos))
	for i, r := range t.Repos {
		names[i] = r.Name
	}
	b.WriteString("\n## Maintainer scope\n")
	if t.ScopeAuthor != "" {
		fmt.Fprintf(&b, "Set by %s on the ticket. It is authoritative for where and what to change.\n", t.ScopeAuthor)
	}
	fmt.Fprintf(&b, "- Repositories in scope: %s\n", strings.Join(names, ", "))
	for _, n := range t.Scope {
		fmt.Fprintf(&b, "\n### %s", n.Key)
		if n.Description != "" {
			fmt.Fprintf(&b, " (%s)", n.Description)
		}
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(n.Value))
	}
	if len(t.Scope) == 0 {
		b.WriteString("- No specs or hints given: work from the issue.\n")
	}

	b.WriteString("\n## Repositories\n")
	for i, r := range t.Repos {
		where := fmt.Sprintf("`%s`", r.Dir)
		if i == 0 {
			where = "current directory " + where
		}
		fmt.Fprintf(&b, "\n### %s — %s\n", r.Name, where)
		if r.ResumeOf > 0 {
			fmt.Fprintf(&b, "- Continue an interrupted attempt (job %d): its partial changes are already in this worktree as uncommitted edits (see `git status` / `git diff`). Review them, keep what is right, fix what is not, and finish the job.\n", r.ResumeOf)
		}
		if r.TestCmd != "" {
			fmt.Fprintf(&b, "- Test command (run from this directory; the runner re-runs it as evidence): `%s`\n", r.TestCmd)
		} else {
			b.WriteString("- No test command configured: build/compile the project if you can to check your change.\n")
		}
		if e := r.E2E; e != nil {
			b.WriteString("- E2E web verification: REQUIRED (follow the E2E rules).\n")
			fmt.Fprintf(&b, "  - App: %s\n", e.BaseURL)
			if e.StartCmd != "" {
				fmt.Fprintf(&b, "  - Start (from this directory): `%s`\n", e.StartCmd)
			}
			fmt.Fprintf(&b, "  - Credentials profile: `%s`\n  - Evidence dir: `%s`\n", e.Profile, e.EvidenceDir)
			if n := strings.TrimSpace(e.Notes); n != "" {
				fmt.Fprintf(&b, "  - Notes: %s\n", n)
			}
		} else {
			b.WriteString("- E2E web verification: not configured for this repo (write \"not configured\" under E2E).\n")
		}
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no description)"
	}
	return s
}
