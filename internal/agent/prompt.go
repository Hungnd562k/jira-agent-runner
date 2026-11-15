package agent

import (
	"fmt"
	"strings"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
)

// Task is the per-job context added to the prompt next to the issue.
type Task struct {
	TestCmd string
	E2E     *E2E // nil = no browser verification for this repo
	// ResumeOf is the interrupted job whose partial changes are already in the working tree (0 = fresh).
	ResumeOf int64
}

// E2E describes the local web app the agent must verify in a browser.
type E2E struct {
	BaseURL     string
	StartCmd    string
	Profile     string // credentials profile name
	Notes       string // repo-specific hints (login flow, menus, test data)
	EvidenceDir string // absolute, outside the worktree
}

// BuildPrompt turns a Jira issue into the task for the coding agent.
// The standing rules (Rules) go into the system prompt; this is the job itself.
func BuildPrompt(d jira.IssueDetail, t Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fix Jira issue %s in this repository (current directory, a fresh git branch). Follow the bug-fix rules in your system prompt.\n\n", d.Key)
	fmt.Fprintf(&b, "## Issue\n- Key: %s\n- Type: %s\n- Priority: %s\n- Status: %s\n", d.Key, d.Type, d.Priority, d.Status)
	if len(d.Components) > 0 {
		fmt.Fprintf(&b, "- Components: %s\n", strings.Join(d.Components, ", "))
	}
	if len(d.Labels) > 0 {
		fmt.Fprintf(&b, "- Labels: %s\n", strings.Join(d.Labels, ", "))
	}
	fmt.Fprintf(&b, "- Summary: %s\n\n### Description\n%s\n", d.Summary, orNone(d.Description))
	if len(d.Comments) > 0 {
		b.WriteString("\n### Latest comments\n")
		for _, c := range d.Comments {
			fmt.Fprintf(&b, "- %s (%s): %s\n", c.Author, c.Created.Format("2006-01-02"), strings.TrimSpace(c.Body))
		}
	}

	b.WriteString("\n## Job\n")
	if t.ResumeOf > 0 {
		fmt.Fprintf(&b, "- Continue an interrupted attempt (job %d): its partial changes are already in the working tree as uncommitted edits (see `git status` / `git diff`). Review them, keep what is right, fix what is not, and finish the job.\n", t.ResumeOf)
	}
	if t.TestCmd != "" {
		fmt.Fprintf(&b, "- Test command (the runner re-runs it as evidence): `%s`\n", t.TestCmd)
	} else {
		b.WriteString("- No test command configured: build/compile the project if you can to check your change.\n")
	}
	if e := t.E2E; e != nil {
		b.WriteString("- E2E web verification: REQUIRED (follow the E2E rules).\n")
		fmt.Fprintf(&b, "  - App: %s\n", e.BaseURL)
		if e.StartCmd != "" {
			fmt.Fprintf(&b, "  - Start: `%s`\n", e.StartCmd)
		}
		fmt.Fprintf(&b, "  - Credentials profile: `%s`\n  - Evidence dir: `%s`\n", e.Profile, e.EvidenceDir)
		if n := strings.TrimSpace(e.Notes); n != "" {
			fmt.Fprintf(&b, "  - Notes: %s\n", n)
		}
	} else {
		b.WriteString("- E2E web verification: not configured for this repo (write \"not configured\" under E2E).\n")
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no description)"
	}
	return s
}
