package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// TestRun is the evidence produced by running the repo's test command.
type TestRun struct {
	Passed   bool
	ExitCode int
	Duration time.Duration
	Tail     string // last lines of output, for MR description / Telegram
}

// RunTests executes cmd via `sh -c` in dir, writing full output to logPath.
// A failing test command is not an error; only failure to start or a timeout is.
func RunTests(ctx context.Context, dir, cmd, logPath string, timeout time.Duration) (TestRun, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	f, err := os.Create(logPath)
	if err != nil {
		return TestRun{}, err
	}
	defer f.Close()
	fmt.Fprintf(f, "$ %s\n", cmd)

	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = dir
	c.Stdout, c.Stderr = f, f
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 10 * time.Second

	start := time.Now()
	runErr := c.Run()
	tr := TestRun{Duration: time.Since(start)}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return tr, fmt.Errorf("tests timed out after %s", timeout)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		tr.Passed = true
	case errors.As(runErr, &exitErr):
		tr.ExitCode = exitErr.ExitCode()
	default:
		return tr, fmt.Errorf("run tests: %w", runErr)
	}
	fmt.Fprintf(f, "\n[exit %d after %s]\n", tr.ExitCode, tr.Duration.Round(time.Second))
	f.Sync()
	if b, err := os.ReadFile(logPath); err == nil {
		tr.Tail = lastLines(string(b), 40)
	}
	return tr, nil
}
