// Package agent prepares git worktrees, runs the coding agent (Claude Code CLI) and the
// repository's tests, and commits/pushes the result.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs git commands against a repository directory.
type Git struct{ Dir string }

// Run executes `git <args>` in g.Dir and returns trimmed combined output.
func (g Git) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	// Never block on credential/editor prompts in an unattended worker.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, lastLines(s, 5))
	}
	return s, nil
}

// Worktree is an isolated checkout of a new branch for one job.
type Worktree struct {
	Repo   Git    // the main clone
	Dir    string // worktree path
	Branch string
}

// NewWorktree fetches origin and creates branch from origin/<base> in a fresh worktree under root.
func NewWorktree(ctx context.Context, repoPath, root, name, branch, base string) (*Worktree, error) {
	repo := Git{Dir: repoPath}
	if _, err := repo.Run(ctx, "fetch", "--prune", "origin", base); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, name)
	_ = os.RemoveAll(dir) // stale leftovers from a crashed run
	_, _ = repo.Run(ctx, "worktree", "prune")
	if _, err := repo.Run(ctx, "worktree", "add", "-b", branch, dir, "origin/"+base); err != nil {
		return nil, err
	}
	return &Worktree{Repo: repo, Dir: dir, Branch: branch}, nil
}

// Git returns a runner bound to the worktree.
func (w *Worktree) Git() Git { return Git{Dir: w.Dir} }

// HasChanges reports whether the agent left any modification (tracked or untracked).
func (w *Worktree) HasChanges(ctx context.Context) (bool, error) {
	out, err := w.Git().Run(ctx, "status", "--porcelain")
	return out != "", err
}

// DiffStat summarizes pending changes (for the MR description).
func (w *Worktree) DiffStat(ctx context.Context) string {
	_, _ = w.Git().Run(ctx, "add", "-A")
	out, _ := w.Git().Run(ctx, "diff", "--cached", "--stat")
	return out
}

// Commit stages everything and commits with message (author = host git identity).
func (w *Worktree) Commit(ctx context.Context, message string) error {
	if _, err := w.Git().Run(ctx, "add", "-A"); err != nil {
		return err
	}
	_, err := w.Git().Run(ctx, "commit", "--no-verify", "-m", message)
	return err
}

// Push publishes the branch to origin.
func (w *Worktree) Push(ctx context.Context) error {
	_, err := w.Git().Run(ctx, "push", "--set-upstream", "origin", w.Branch)
	return err
}

// Remove deletes the worktree directory; the branch (and its commit) is kept.
func (w *Worktree) Remove(ctx context.Context) error {
	_, err := w.Repo.Run(ctx, "worktree", "remove", "--force", w.Dir)
	return err
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
