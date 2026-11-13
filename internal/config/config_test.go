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
