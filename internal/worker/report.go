package worker

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

// Report is what a finished job tells humans: where the change is and what the agent did.
type Report struct {
	Key, Summary, IssueURL string
	Repo                   repos.Repo
	Branch                 string
	JobDir                 string
	DiffStat               string
	Tests                  *agent.TestRun
	Agent                  agent.ClaudeResult
	MRURL                  string
}

func (r *Report) ready() bool { return r.Branch != "" && r.Agent.Text != "" }

func (r *Report) testsLine() string {
	switch {
	case r.Tests == nil:
		return "not run (no test_cmd configured)"
	case r.Tests.Passed:
		return fmt.Sprintf("✅ passed — `%s` (%s)", r.Repo.TestCmd, r.Tests.Duration.Round(time.Second))
	default:
		return fmt.Sprintf("❌ failed — `%s` exited %d", r.Repo.TestCmd, r.Tests.ExitCode)
	}
}

// Telegram renders the report as Telegram HTML (kept under the 4096-char message limit).
func (r *Report) Telegram() string {
	var b strings.Builder
	fmt.Fprintf(&b, "📋 <b>%s</b> · <code>%s</code>\n", html.EscapeString(r.Key), html.EscapeString(r.Repo.Name))
	if r.MRURL != "" {
		fmt.Fprintf(&b, "🔀 <a href=\"%s\">Draft MR</a> · branch <code>%s</code>\n", html.EscapeString(r.MRURL), html.EscapeString(r.Branch))
	} else {
		fmt.Fprintf(&b, "🌿 Local branch <code>%s</code> (not pushed)\n👀 <code>git -C %s show %s</code>\n",
			html.EscapeString(r.Branch), html.EscapeString(r.Repo.Path), html.EscapeString(r.Branch))
	}
	fmt.Fprintf(&b, "🧪 Tests: %s\n", html.EscapeString(r.testsLine()))
	fmt.Fprintf(&b, "💰 $%.2f · %d turns · logs <code>%s</code>\n", r.Agent.CostUSD, r.Agent.NumTurns, html.EscapeString(r.JobDir))
	if r.DiffStat != "" {
		b.WriteString("\n<pre>" + html.EscapeString(truncate(r.DiffStat, 600)) + "</pre>\n")
	}
	head := b.String()
	room := 3900 - len(head)
	if room < 200 {
		room = 200
	}
	return head + "\n<b>Agent report</b>\n" + html.EscapeString(truncate(strings.TrimSpace(r.Agent.Text), room))
}

// Markdown renders the full report for data/jobs/<id>/report.md.
func (r *Report) Markdown(j store.Job) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", r.Key, r.Summary)
	fmt.Fprintf(&b, "- Jira: %s\n- Repo: %s (`%s`)\n- Branch: `%s`\n", r.IssueURL, r.Repo.Name, r.Repo.Path, r.Branch)
	if r.MRURL != "" {
		fmt.Fprintf(&b, "- Draft MR: %s\n", r.MRURL)
	}
	fmt.Fprintf(&b, "- Job: #%d %s\n- Tests: %s\n- Agent: $%.2f, %d turns\n\n", j.ID, j.Status, r.testsLine(), r.Agent.CostUSD, r.Agent.NumTurns)
	if r.DiffStat != "" {
		b.WriteString("## Diff stat\n\n```\n" + r.DiffStat + "\n```\n\n")
	}
	b.WriteString("## Agent report\n\n" + strings.TrimSpace(r.Agent.Text) + "\n")
	if r.Tests != nil {
		b.WriteString("\n## Test output (tail)\n\n```\n" + r.Tests.Tail + "\n```\n")
	}
	return b.String()
}

// JiraComment renders the comment posted on the issue (Jira Server wiki markup).
func (r *Report) JiraComment() string {
	var b strings.Builder
	b.WriteString("(*) *jira-agent-runner*: an AI agent (Claude Code) prepared a fix for review.\n\n")
	fmt.Fprintf(&b, "* Draft MR: %s\n* Repo: %s, branch {{%s}}\n", r.MRURL, r.Repo.Name, r.Branch)
	tests := strings.NewReplacer("✅", "(/)", "❌", "(x)", "`", "").Replace(r.testsLine())
	fmt.Fprintf(&b, "* Tests: %s\n\n", tests)
	b.WriteString("{noformat}\n" + truncate(strings.TrimSpace(r.Agent.Text), 6000) + "\n{noformat}\n")
	b.WriteString("_Not merged — a human must review the MR._")
	return b.String()
}
