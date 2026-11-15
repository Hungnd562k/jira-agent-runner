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
	p := BuildPrompt(d, Task{Repos: []RepoTask{{Name: "app", Dir: "/wt/app", TestCmd: "go test ./..."}}})
	for _, want := range []string{"`go test ./...`", "not configured for this repo", "### app — current directory `/wt/app`",
		"Repositories in scope: app", "No specs or hints"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt without e2e misses %q:\n%s", want, p)
		}
	}
	p = BuildPrompt(d, Task{Repos: []RepoTask{{Name: "web", E2E: &E2E{BaseURL: "http://localhost:8803", Profile: "local-maker", EvidenceDir: "/ev"}}}})
	for _, want := range []string{"REQUIRED", "http://localhost:8803", "`local-maker`", "`/ev`"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt misses %q", want)
		}
	}
}

func TestBuildPromptMultiRepoScope(t *testing.T) {
	var cs []jira.Comment
	for i := range 8 {
		cs = append(cs, jira.Comment{Author: "QA", Body: "comment-" + string(rune('a'+i))})
	}
	d := jira.IssueDetail{Issue: jira.Issue{Key: "X-1", Summary: "Crash"}, Type: "Bug", Comments: cs}
	task := Task{
		Repos:       []RepoTask{{Name: "host", Dir: "/wt/host"}, {Name: "web", Dir: "/wt/web", ResumeOf: 3}},
		Scope:       []ScopeNote{{Key: "specs", Description: "yêu cầu", Value: "Thêm cột X"}, {Key: "hints", Value: "xem Foo.cs"}},
		ScopeAuthor: "Hung",
	}
	if task.HasE2E() {
		t.Error("HasE2E without e2e repos")
	}
	p := BuildPrompt(d, task)
	for _, want := range []string{"Repositories in scope: host, web", "Set by Hung", "### specs (yêu cầu)\nThêm cột X",
		"### hints\nxem Foo.cs", "### web — `/wt/web`", "interrupted attempt (job 3)", "comment-h"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt misses %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "comment-c") {
		t.Error("prompt keeps more than the latest 5 comments")
	}
	if !strings.Contains(Rules(false), "Scope (guard rules)") {
		t.Error("rules miss the scope guard")
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
	if p := BuildPrompt(d, Task{Repos: []RepoTask{{Name: "app"}}}); strings.Contains(p, "interrupted attempt") {
		t.Error("fresh job mentions a resume")
	}
	if p := BuildPrompt(d, Task{Repos: []RepoTask{{Name: "app", ResumeOf: 7}}}); !strings.Contains(p, "interrupted attempt (job 7)") {
		t.Errorf("resume prompt = %s", p)
	}
}
