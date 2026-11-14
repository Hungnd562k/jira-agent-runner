package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// WorkerConfig holds settings only the host-side agent worker needs.
type WorkerConfig struct {
	GitLabBaseURL string
	GitLabToken   string

	WorktreeDir       string
	ClaudeBin         string
	AgentModel        string
	AgentTimeout      time.Duration
	AgentMaxBudgetUSD float64
	TestTimeout       time.Duration

	// E2E: web login profiles for browser verification (git-ignored, never logged).
	E2ECredentialsFile string // absolute
	E2ENodePath        string // NODE_PATH for the agent's Playwright scripts (optional)

	// Handover (dev → test) after fixes are merged into dev.
	HandoverMinTickets   int
	PipelinePoll         time.Duration
	PipelineStartTimeout time.Duration
	JiraHandoverStatus   string
	JiraResolution       string
}

// LoadWorker reads worker settings. GitLab settings are required unless requireGitLab is false (--no-push).
func LoadWorker(requireGitLab bool) (*WorkerConfig, error) {
	w := &WorkerConfig{
		GitLabBaseURL: os.Getenv("GITLAB_BASE_URL"),
		GitLabToken:   os.Getenv("GITLAB_TOKEN"),
		WorktreeDir:   os.Getenv("WORKTREE_DIR"), // empty = next to each repo clone
		ClaudeBin:     getenv("CLAUDE_BIN", "claude"),
		AgentModel:    os.Getenv("AGENT_MODEL"),
		E2ENodePath:   os.Getenv("E2E_NODE_PATH"),

		JiraHandoverStatus: getenv("JIRA_HANDOVER_STATUS", "Resolved"),
		JiraResolution:     getenv("JIRA_RESOLUTION", "Fixed"),
	}
	var errs []error
	var err error
	if w.AgentTimeout, err = durationEnv("AGENT_TIMEOUT", "30m"); err != nil {
		errs = append(errs, err)
	}
	if w.TestTimeout, err = durationEnv("TEST_TIMEOUT", "15m"); err != nil {
		errs = append(errs, err)
	}
	if w.AgentMaxBudgetUSD, err = strconv.ParseFloat(getenv("AGENT_MAX_BUDGET_USD", "5"), 64); err != nil || w.AgentMaxBudgetUSD < 0 {
		errs = append(errs, fmt.Errorf("AGENT_MAX_BUDGET_USD must be a number >= 0"))
	}
	if w.WorktreeDir != "" && !filepath.IsAbs(w.WorktreeDir) {
		errs = append(errs, fmt.Errorf("WORKTREE_DIR must be absolute, got %q", w.WorktreeDir))
	}
	if w.HandoverMinTickets, err = strconv.Atoi(getenv("HANDOVER_MIN_TICKETS", "1")); err != nil || w.HandoverMinTickets < 1 {
		errs = append(errs, fmt.Errorf("HANDOVER_MIN_TICKETS must be an integer >= 1"))
	}
	if w.PipelinePoll, err = durationEnv("PIPELINE_POLL_INTERVAL", "1m"); err != nil {
		errs = append(errs, err)
	}
	if w.PipelineStartTimeout, err = durationEnv("PIPELINE_START_TIMEOUT", "15m"); err != nil {
		errs = append(errs, err)
	}
	if w.E2ECredentialsFile, err = filepath.Abs(getenv("E2E_CREDENTIALS_FILE", ".secrets/credentials.json")); err != nil {
		errs = append(errs, fmt.Errorf("E2E_CREDENTIALS_FILE: %w", err))
	}
	if requireGitLab {
		if err := validateRootURL("GITLAB_BASE_URL", w.GitLabBaseURL); err != nil {
			errs = append(errs, err)
		}
		if w.GitLabToken == "" {
			errs = append(errs, errors.New("GITLAB_TOKEN is required (or run the worker with --no-push)"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return w, nil
}
