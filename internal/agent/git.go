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

// SaveWIP commits whatever the agent left uncommitted (a failed or interrupted run) so it
// survives the worktree's removal; the commit stays on the local branch and is never pushed.
// sha is "" when there was nothing to save.
func (w *Worktree) SaveWIP(ctx context.Context, message string) (sha string, err error) {
	changed, err := w.HasChanges(ctx)
	if err != nil || !changed {
		return "", err
	}
	if err := w.Commit(ctx, message); err != nil {
		return "", err
	}
	return w.Git().Run(ctx, "rev-parse", "HEAD")
}

// ApplyWIP brings a saved WIP commit back as uncommitted changes, on top of the current
// HEAD (the fresh base) when it applies cleanly, otherwise on the base it was made from.
func (w *Worktree) ApplyWIP(ctx context.Context, sha string) error {
	g := w.Git()
	if _, err := g.Run(ctx, "cherry-pick", "--no-commit", sha); err == nil {
		_, err = g.Run(ctx, "reset", "--quiet") // unstage: the agent sees plain local edits
		return err
	}
	_, _ = g.Run(ctx, "cherry-pick", "--abort")
	_, _ = g.Run(ctx, "reset", "--hard", "--quiet")
	if _, err := g.Run(ctx, "reset", "--hard", "--quiet", sha+"^"); err != nil {
		return err
	}
	if _, err := g.Run(ctx, "cherry-pick", "--no-commit", sha); err != nil {
		return err
	}
	_, err := g.Run(ctx, "reset", "--quiet")
	return err
}

// ListedWorktree is one entry of `git worktree list --porcelain`.
type ListedWorktree struct {
	Dir    string
	Branch string // short name, "" when detached
}

// Worktrees lists the linked worktrees of the repository (the main clone excluded).
func (g Git) Worktrees(ctx context.Context) ([]ListedWorktree, error) {
	out, err := g.Run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var all []ListedWorktree
	for _, block := range strings.Split(out, "\n\n") {
		var wt ListedWorktree
		for _, l := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(l, "worktree "); ok {
				wt.Dir = v
			} else if v, ok := strings.CutPrefix(l, "branch refs/heads/"); ok {
				wt.Branch = v
			}
		}
		if wt.Dir != "" {
			all = append(all, wt)
		}
	}
	if len(all) > 0 {
		all = all[1:] // the first entry is the main working tree
	}
	return all, nil
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

// CopyIgnoredFiles copies the named files from src to dst when they exist in src and are
// absent in dst (e.g. git-ignored CLAUDE.md that a fresh worktree does not have).
// Being ignored, they are never picked up by `git add -A`.
func CopyIgnoredFiles(src, dst string, names ...string) {
	for _, n := range names {
		to := filepath.Join(dst, n)
		if _, err := os.Stat(to); err == nil {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(src, n)); err == nil {
			_ = os.WriteFile(to, b, 0o644)
		}
	}
}
