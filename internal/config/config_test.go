package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("JIRA_BASE_URL", "https://jira.example.com/")
	t.Setenv("JIRA_PAT", "x")

	c, err := Load(false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JiraBaseURL != "https://jira.example.com" {
		t.Errorf("base URL not trimmed: %q", c.JiraBaseURL)
	}
	if c.PollInterval != 5*time.Minute || c.RemindInterval != 4*time.Hour {
		t.Errorf("unexpected intervals: %v %v", c.PollInterval, c.RemindInterval)
	}
	if len(c.SnoozeChoices) != 3 {
		t.Errorf("snooze choices: %v", c.SnoozeChoices)
	}
}

func TestLoadRequiresTelegram(t *testing.T) {
	t.Setenv("JIRA_BASE_URL", "https://jira.example.com")
	t.Setenv("JIRA_PAT", "x")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")

	if _, err := Load(true); err == nil {
		t.Fatal("expected error when telegram settings missing")
	}
}

func TestParseDurationsRejectsInvalid(t *testing.T) {
	for _, in := range []string{"", "abc", "-1m", "1m,,0s"} {
		if _, err := parseDurations(in); err == nil {
			t.Errorf("parseDurations(%q) expected error", in)
		}
	}
}

func TestValidateBaseURL(t *testing.T) {
	ok := []string{"https://jira.example.com", "https://example.com/jira"}
	bad := []string{"", "jira.example.com", "https://jira.example.com/secure/RapidBoard.jspa?rapidView=1",
		"https://jira.example.com/browse/X-1", "https://jira.example.com/?a=b"}
	for _, u := range ok {
		if err := validateBaseURL(u); err != nil {
			t.Errorf("validateBaseURL(%q) = %v", u, err)
		}
	}
	for _, u := range bad {
		if err := validateBaseURL(u); err == nil {
			t.Errorf("validateBaseURL(%q) expected error", u)
		}
	}
}

func TestLoadWorker(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("WORKTREE_DIR", "/tmp/wt")
	if _, err := LoadWorker(true); err == nil {
		t.Fatal("expected GitLab settings to be required")
	}
	w, err := LoadWorker(false)
	if err != nil || w.AgentTimeout != 30*time.Minute || w.AgentMaxBudgetUSD != 5 || w.ClaudeBin != "claude" {
		t.Fatalf("LoadWorker(no-push) = %+v %v", w, err)
	}
	t.Setenv("GITLAB_BASE_URL", "https://git.example.com/group/proj/-/merge_requests")
	t.Setenv("GITLAB_TOKEN", "x")
	if _, err := LoadWorker(true); err == nil {
		t.Fatal("page URL should be rejected")
	}
	t.Setenv("GITLAB_BASE_URL", "https://git.example.com")
	if _, err := LoadWorker(true); err != nil {
		t.Fatal(err)
	}
}
