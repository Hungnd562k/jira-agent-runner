package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git{Dir: dir}.Run(context.Background(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSaveAndApplyWIP: WIP made on an old base comes back as uncommitted edits, on the new
// base when it applies cleanly and on its own base when it conflicts.
func TestSaveAndApplyWIP(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@e", "GIT_COMMITTER_NAME": "t",
		"GIT_COMMITTER_EMAIL": "t@e", "GIT_CONFIG_GLOBAL": "/dev/null"} {
		t.Setenv(k, v)
	}
	ctx := context.Background()
	root := t.TempDir()
	origin, clone := filepath.Join(root, "origin.git"), filepath.Join(root, "clone")
	run(t, root, "init", "--bare", "-b", "main", origin)
	run(t, root, "clone", origin, clone)
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b\n"), 0o644)
	run(t, clone, "add", "-A")
	run(t, clone, "commit", "-m", "init")
	run(t, clone, "push", "origin", "HEAD:main")

	wt, err := NewWorktree(ctx, clone, filepath.Join(root, "wt"), "j1", "agent/X-1-j1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if sha, err := wt.SaveWIP(ctx, "wip"); err != nil || sha != "" {
		t.Fatalf("clean worktree saved %q %v", sha, err)
	}
	os.WriteFile(filepath.Join(wt.Dir, "a.txt"), []byte("half\n"), 0o644)
	sha, err := wt.SaveWIP(ctx, "wip")
	if err != nil || sha == "" {
		t.Fatalf("save = %q %v", sha, err)
	}
	if list, _ := (Git{Dir: clone}).Worktrees(ctx); len(list) != 1 || list[0].Branch != "agent/X-1-j1" {
		t.Fatalf("worktrees = %+v", list)
	}
	wt.Remove(ctx)

	// Base moves on with an unrelated change: WIP applies on top of it.
	os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b2\n"), 0o644)
	run(t, clone, "commit", "-am", "b2")
	run(t, clone, "push", "origin", "HEAD:main")
	wt2, _ := NewWorktree(ctx, clone, filepath.Join(root, "wt"), "j2", "agent/X-1-j2", "main")
	if err := wt2.ApplyWIP(ctx, sha); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt2.Dir, "b.txt")); string(got) != "b2\n" {
		t.Errorf("not on the new base: b.txt = %q", got)
	}
	if st := run(t, wt2.Dir, "status", "--porcelain"); st != "M a.txt" {
		t.Errorf("status = %q", st)
	}
	wt2.Remove(ctx)

	// Base changes the same line: WIP falls back to the base it was made from.
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("other\n"), 0o644)
	run(t, clone, "commit", "-am", "a2")
	run(t, clone, "push", "origin", "HEAD:main")
	wt3, _ := NewWorktree(ctx, clone, filepath.Join(root, "wt"), "j3", "agent/X-1-j3", "main")
	if err := wt3.ApplyWIP(ctx, sha); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt3.Dir, "a.txt")); string(got) != "half\n" {
		t.Errorf("a.txt = %q", got)
	}
	if st := run(t, wt3.Dir, "status", "--porcelain"); st != "M a.txt" {
		t.Errorf("status = %q", st)
	}
}
