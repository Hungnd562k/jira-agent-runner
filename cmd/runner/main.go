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
	"path/filepath"
	"slices"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Hungnd562k/jira-agent-runner/internal/config"
	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/scheduler"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
	"github.com/Hungnd562k/jira-agent-runner/internal/telegram"

	_ "time/tzdata" // embed zoneinfo so TZ works in distroless
)

func main() {
	once := flag.Bool("once", false, "run a single poll cycle and exit")
	dryRun := flag.Bool("dry-run", false, "print issues to stdout instead of sending Telegram messages (DB is still updated)")
	envFile := flag.String("env-file", ".env", "dotenv file to load; empty disables (missing file is ignored; real env vars take precedence)")
	healthcheck := flag.Bool("healthcheck", false, "exit 0 if the running service's heartbeat is fresh (for Docker HEALTHCHECK)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *envFile == "" {
		// env already provided (e.g. docker compose env_file)
	} else if err := config.LoadDotEnv(*envFile); err != nil {
		log.Error("load env file", "err", err)
		os.Exit(1)
	}
	if *healthcheck {
		os.Exit(checkHeartbeat(heartbeatPath()))
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

	if dryRun {
		res, err := syncer.Sync(ctx)
		if err != nil {
			return err
		}
		printResult(os.Stdout, res)
		return nil
	}

	svc := &scheduler.Service{
		Syncer:        syncer,
		Store:         st,
		TG:            telegram.New(telegramAPI, cfg.TelegramBotToken),
		ChatID:        cfg.TelegramChatID,
		SnoozeChoices: cfg.SnoozeChoices,
		PollInterval:  cfg.PollInterval,
		RemindAfter:   cfg.RemindInterval,
		RemindCheck:   time.Minute,
		SendGap:       time.Second,
		Now:           time.Now,
		Log:           log,
	}
	hb := heartbeatPath()
	svc.Heartbeat = func() {
		if err := os.WriteFile(hb, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
			log.Warn("write heartbeat", "err", err)
		}
	}
	if once {
		return svc.Cycle(ctx)
	}
	err = svc.Run(ctx)
	log.Info("shutting down")
	return err
}

const telegramAPI = "https://api.telegram.org"

// heartbeatMaxAge must exceed the 1m remind tick that refreshes the heartbeat.
const heartbeatMaxAge = 3 * time.Minute

// heartbeatPath lives next to the database so it shares the /data volume.
func heartbeatPath() string {
	db := os.Getenv("DB_PATH")
	if db == "" {
		db = "./data/tasks.db"
	}
	return filepath.Join(filepath.Dir(db), "heartbeat")
}

func checkHeartbeat(path string) int {
	fi, err := os.Stat(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	if age := time.Since(fi.ModTime()); age > heartbeatMaxAge {
		fmt.Fprintf(os.Stderr, "unhealthy: heartbeat is %s old\n", age.Round(time.Second))
		return 1
	}
	return 0
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
