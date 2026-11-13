// Package config loads and validates runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultJQL = "assignee = currentUser() AND issuetype = Bug AND resolution = Unresolved ORDER BY priority DESC"

// Config holds all runtime settings. See .env.example for the env var names.
type Config struct {
	JiraBaseURL string
	JiraPAT     string
	JiraJQL     string

	TelegramBotToken string
	TelegramChatID   int64

	PollInterval   time.Duration
	RemindInterval time.Duration
	SnoozeChoices  []time.Duration

	DBPath string
}

// Load reads configuration from the environment and validates it.
// When requireTelegram is false (e.g. --dry-run), Telegram settings are optional.
func Load(requireTelegram bool) (*Config, error) {
	c := &Config{
		JiraBaseURL:      strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/"),
		JiraPAT:          os.Getenv("JIRA_PAT"),
		JiraJQL:          getenv("JIRA_JQL", defaultJQL),
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		DBPath:           getenv("DB_PATH", "./data/tasks.db"),
	}

	var errs []error
	var err error

	if c.PollInterval, err = durationEnv("POLL_INTERVAL", "5m"); err != nil {
		errs = append(errs, err)
	}
	if c.RemindInterval, err = durationEnv("REMIND_INTERVAL", "4h"); err != nil {
		errs = append(errs, err)
	}
	if c.SnoozeChoices, err = parseDurations(getenv("SNOOZE_CHOICES", "30m,2h,24h")); err != nil {
		errs = append(errs, fmt.Errorf("SNOOZE_CHOICES: %w", err))
	}

	if c.JiraBaseURL == "" {
		errs = append(errs, errors.New("JIRA_BASE_URL is required"))
	}
	if c.JiraPAT == "" {
		errs = append(errs, errors.New("JIRA_PAT is required"))
	}

	if raw := os.Getenv("TELEGRAM_CHAT_ID"); raw != "" {
		if c.TelegramChatID, err = strconv.ParseInt(raw, 10, 64); err != nil {
			errs = append(errs, fmt.Errorf("TELEGRAM_CHAT_ID: %w", err))
		}
	}
	if requireTelegram {
		if c.TelegramBotToken == "" {
			errs = append(errs, errors.New("TELEGRAM_BOT_TOKEN is required"))
		}
		if c.TelegramChatID == 0 {
			errs = append(errs, errors.New("TELEGRAM_CHAT_ID is required"))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationEnv(key, def string) (time.Duration, error) {
	d, err := time.ParseDuration(getenv(key, def))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be > 0", key)
	}
	return d, nil
}

func parseDurations(s string) ([]time.Duration, error) {
	var out []time.Duration
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		d, err := time.ParseDuration(part)
		if err != nil {
			return nil, err
		}
		if d <= 0 {
			return nil, fmt.Errorf("duration %q must be > 0", part)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one duration required")
	}
	return out, nil
}
