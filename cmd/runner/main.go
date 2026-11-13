// Command runner polls Jira for assigned bugs and asks for approval via Telegram.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/config"
	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/scheduler"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

func main() {
	once := flag.Bool("once", false, "run a single poll cycle and exit")
	dryRun := flag.Bool("dry-run", false, "print issues to stdout instead of sending Telegram messages (DB is still updated)")
	envFile := flag.String("env-file", ".env", "dotenv file to load (missing file is ignored; real env vars take precedence)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := config.LoadDotEnv(*envFile); err != nil {
		log.Error("load env file", "err", err)
		os.Exit(1)
	}
	if err := run(log, *once, *dryRun); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, once, dryRun bool) error {
	cfg, err := config.Load(!dryRun)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	syncer := &scheduler.Syncer{
		Source: jira.New(cfg.JiraBaseURL, cfg.JiraPAT),
		Store:  st,
		JQL:    cfg.JiraJQL,
		Now:    time.Now,
	}

	log.Info("jira-agent-runner starting", "once", once, "dry_run", dryRun,
		"jira", cfg.JiraBaseURL, "db", cfg.DBPath, "poll", cfg.PollInterval)

	res, err := syncer.Sync(ctx)
	if err != nil {
		return err
	}
	log.Info("sync done", "issues", len(res.Issues), "created", len(res.Created),
		"updated", len(res.Updated), "reopened", len(res.Reopened), "closed", len(res.Closed))

	if dryRun {
		printResult(os.Stdout, res)
	}
	if !once {
		// TODO(M2/M3): Telegram notifier + poll/remind loops.
		log.Warn("long-running mode not implemented yet (M3); exiting after one cycle")
	}
	return nil
}

func printResult(w io.Writer, res *scheduler.SyncResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHANGE\tKEY\tPRIORITY\tSTATUS\tSUMMARY\tURL")
	for _, iss := range res.Issues {
		change := ""
		switch {
		case slices.Contains(res.Created, iss.Key):
			change = "NEW"
		case slices.Contains(res.Reopened, iss.Key):
			change = "REOPENED"
		case slices.Contains(res.Updated, iss.Key):
			change = "UPDATED"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", change, iss.Key, iss.Priority, iss.Status, truncate(iss.Summary, 60), iss.URL)
	}
	for _, t := range res.Closed {
		fmt.Fprintf(tw, "CLOSED\t%s\t%s\t%s\t%s\t%s\n", t.JiraKey, t.Priority, t.JiraStatus, truncate(t.Summary, 60), t.URL)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d issue(s) matching JQL, %d closed\n", len(res.Issues), len(res.Closed))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
