package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/agent"
	"github.com/Hungnd562k/jira-agent-runner/internal/config"
	"github.com/Hungnd562k/jira-agent-runner/internal/gitlab"
	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"
	"github.com/Hungnd562k/jira-agent-runner/internal/worker"
)

// runWorker processes approved agent jobs on the host (needs claude, git credentials, toolchains).
func runWorker(log *slog.Logger, once, noPush bool, publishJob int64) error {
	cfg, err := config.Load(true)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	wcfg, err := config.LoadWorker(!noPush)
	if err != nil {
		return fmt.Errorf("invalid worker configuration: %w", err)
	}
	if _, err := exec.LookPath(wcfg.ClaudeBin); err != nil {
		return fmt.Errorf("CLAUDE_BIN %q not found: %w", wcfg.ClaudeBin, err)
	}
	repoCfg, err := repos.Load(cfg.ReposFile)
	if err != nil {
		return fmt.Errorf("load repos: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	jc := jira.New(cfg.JiraBaseURL, cfg.JiraPAT)
	w := &worker.Worker{
		Store:  st,
		Repos:  repoCfg,
		Jira:   jc,
		TG:     telegram.New(telegramAPI, cfg.TelegramBotToken),
		ChatID: cfg.TelegramChatID,
		NoPush: noPush,
		Claude: agent.ClaudeSpec{
			Bin:          wcfg.ClaudeBin,
			MaxBudgetUSD: wcfg.AgentMaxBudgetUSD,
			Model:        wcfg.AgentModel,
			Timeout:      wcfg.AgentTimeout,
		},
		TestTimeout:     wcfg.TestTimeout,
		CredentialsFile: wcfg.E2ECredentialsFile,
		NodePath:        wcfg.E2ENodePath,
		WorktreeRoot:    wcfg.WorktreeDir,
		JobsDir:         filepath.Join(filepath.Dir(cfg.DBPath), "jobs"),
		Now:             time.Now,
		Log:             log,
	}
	if !noPush {
		w.MR = gitlab.New(wcfg.GitLabBaseURL, wcfg.GitLabToken)
		w.Comment = jc // Jira comment only when a Draft MR is opened
	}

	log.Info("worker starting", "once", once, "no_push", noPush, "db", cfg.DBPath,
		"repos_projects", len(repoCfg), "worktrees", wcfg.WorktreeDir, "agent_timeout", wcfg.AgentTimeout,
		"max_budget_usd", wcfg.AgentMaxBudgetUSD)

	if publishJob > 0 {
		return w.Publish(ctx, publishJob)
	}
	if once {
		did, err := w.RunOnce(ctx)
		if err == nil && !did {
			log.Info("no queued job")
		}
		return err
	}
	err = w.Run(ctx)
	log.Info("worker stopped")
	return err
}
