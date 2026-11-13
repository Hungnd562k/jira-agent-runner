// Command runner polls Jira for assigned bugs and asks for approval via Telegram.
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/Hungnd562k/jira-agent-runner/internal/config"
)

func main() {
	once := flag.Bool("once", false, "run a single poll cycle and exit")
	dryRun := flag.Bool("dry-run", false, "print issues to stdout instead of sending Telegram messages")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(!*dryRun)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	log.Info("jira-agent-runner starting",
		"once", *once, "dry_run", *dryRun,
		"jira", cfg.JiraBaseURL, "poll", cfg.PollInterval, "remind", cfg.RemindInterval)

	// TODO(M1): jira client + store; TODO(M2): telegram; TODO(M3): scheduler loops.
}
