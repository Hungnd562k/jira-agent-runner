package agent

import (
	"strings"
	"testing"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
)

func TestRules(t *testing.T) {
	base := Rules(false)
	if !strings.Contains(base, "Bug-fix rules") || strings.Contains(base, "E2E web verification rules") {
		t.Errorf("Rules(false) = %q", base[:80])
	}
	if !strings.Contains(Rules(true), "E2E web verification rules") {
		t.Error("Rules(true) misses the E2E rules")
	}
}

func TestBuildPrompt(t *testing.T) {
	d := jira.IssueDetail{Issue: jira.Issue{Key: "X-1", Summary: "Crash"}, Type: "Bug"}
	p := BuildPrompt(d, Task{TestCmd: "go test ./..."})
	if !strings.Contains(p, "`go test ./...`") || !strings.Contains(p, "not configured for this repo") {
		t.Errorf("prompt without e2e = %s", p)
	}
	p = BuildPrompt(d, Task{E2E: &E2E{BaseURL: "http://localhost:8803", Profile: "local-maker", EvidenceDir: "/ev"}})
	for _, want := range []string{"REQUIRED", "http://localhost:8803", "`local-maker`", "`/ev`"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt misses %q", want)
		}
	}
}

func TestArgs(t *testing.T) {
	a := strings.Join(ClaudeSpec{SystemPrompt: "rules", AddDirs: []string{"/ev"}, Disallowed: []string{"Read(//c.json)"}}.Args(), " ")
	for _, want := range []string{"--append-system-prompt rules", "--add-dir /ev", "--disallowedTools Read(//c.json)"} {
		if !strings.Contains(a, want) {
			t.Errorf("args %q miss %q", a, want)
		}
	}
}

func TestBuildPromptResume(t *testing.T) {
	d := jira.IssueDetail{Issue: jira.Issue{Key: "X-1", Summary: "Crash"}, Type: "Bug"}
	if p := BuildPrompt(d, Task{}); strings.Contains(p, "interrupted attempt") {
		t.Error("fresh job mentions a resume")
	}
	if p := BuildPrompt(d, Task{ResumeOf: 7}); !strings.Contains(p, "interrupted attempt (job 7)") {
		t.Errorf("resume prompt = %s", p)
	}
}
