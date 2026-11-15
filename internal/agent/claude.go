package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// BaseTools are always granted: read/edit files and inspect (not mutate) git state.
// Git writes (commit/push) are done by the worker, never by the agent.
var BaseTools = []string{"Read", "Edit", "Write", "Glob", "Grep", "Bash(git diff:*)", "Bash(git status:*)", "Bash(git log:*)"}

// ClaudeSpec configures one headless Claude Code run.
type ClaudeSpec struct {
	Bin          string // default "claude"
	Dir          string // working directory (the worktree)
	Prompt       string
	AllowedTools []string // appended to BaseTools
	// Disallowed are deny rules, e.g. "Read(//abs/secret.json)" (deny wins over allow).
	Disallowed   []string
	SystemPrompt string   // appended to Claude Code's system prompt (the job rules)
	AddDirs      []string // extra directories the agent may access (e.g. the evidence dir)
	Env          []string // extra KEY=VALUE environment for the agent and its tools
	MaxBudgetUSD float64  // 0 = no limit flag
	Model        string   // optional
	Timeout      time.Duration
	LogPath      string // raw JSON output is written here
	// OnStart, when set, receives the agent's PID (= its process group id) right after start.
	OnStart func(pgid int)
}

// ClaudeResult is the parsed `--output-format json` result.
type ClaudeResult struct {
	Text     string  `json:"result"`
	IsError  bool    `json:"is_error"`
	Subtype  string  `json:"subtype"`
	CostUSD  float64 `json:"total_cost_usd"`
	NumTurns int     `json:"num_turns"`
}

// Args builds the CLI arguments (exported for tests/logging).
func (s ClaudeSpec) Args() []string {
	args := []string{"-p", "--output-format", "json", "--permission-mode", "acceptEdits", "--allowedTools"}
	args = append(args, BaseTools...)
	args = append(args, s.AllowedTools...)
	if len(s.Disallowed) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, s.Disallowed...)
	}
	if len(s.AddDirs) > 0 {
		args = append(args, "--add-dir")
		args = append(args, s.AddDirs...)
	}
	if s.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", s.SystemPrompt)
	}
	if s.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(s.MaxBudgetUSD, 'f', 2, 64))
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	return args
}

// RunClaude executes Claude Code headless in spec.Dir with the prompt on stdin.
// On timeout the whole process group is killed.
func RunClaude(ctx context.Context, spec ClaudeSpec) (ClaudeResult, error) {
	bin := spec.Bin
	if bin == "" {
		bin = "claude"
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, spec.Args()...)
	cmd.Dir = spec.Dir
	cmd.Stdin = strings.NewReader(spec.Prompt)
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Start()
	if runErr == nil {
		if spec.OnStart != nil {
			spec.OnStart(cmd.Process.Pid)
		}
		runErr = cmd.Wait()
	}
	// Reap anything the agent left running in its group (e.g. a local app server started for E2E),
	// so the next job finds the port free.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	if spec.LogPath != "" {
		_ = os.WriteFile(spec.LogPath, append(stdout.Bytes(), append([]byte("\n--- stderr ---\n"), stderr.Bytes()...)...), 0o644)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ClaudeResult{}, fmt.Errorf("agent timed out after %s", spec.Timeout)
	}

	var res ClaudeResult
	if err := json.Unmarshal(lastJSON(stdout.Bytes()), &res); err != nil {
		if runErr != nil {
			return ClaudeResult{}, fmt.Errorf("agent exited: %w: %s", runErr, lastLines(stderr.String(), 3))
		}
		return ClaudeResult{}, fmt.Errorf("agent output is not JSON: %w", err)
	}
	if res.IsError || runErr != nil {
		reason := res.Subtype
		if reason == "" && runErr != nil {
			reason = runErr.Error()
		}
		return res, fmt.Errorf("agent error (%s): %s", reason, firstLine(res.Text))
	}
	return res, nil
}

// BootID identifies the current host boot (Linux); "" when unknown. A process group id is
// only meaningful within the boot that recorded it.
func BootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// KillGroup SIGKILLs a process group; a group that no longer exists is not an error.
func KillGroup(pgid int) error {
	if pgid <= 1 {
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// lastJSON returns the last top-level JSON object line of out (tolerates leading noise).
func lastJSON(out []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if l := bytes.TrimSpace(lines[i]); bytes.HasPrefix(l, []byte("{")) {
			return l
		}
	}
	return bytes.TrimSpace(out)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
