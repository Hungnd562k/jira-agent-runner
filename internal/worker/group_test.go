package worker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

// addRepo creates a second origin + clone ("lib") next to setup's clone.
func addRepo(t *testing.T, clone, name string) string {
	t.Helper()
	root := filepath.Dir(clone)
	origin := filepath.Join(root, name+".git")
	dir := filepath.Join(root, name)
	git(t, root, "init", "--bare", "-b", "main", origin)
	git(t, root, "clone", origin, dir)
	os.WriteFile(filepath.Join(dir, name+".txt"), []byte("buggy\n"), 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", "init")
	git(t, dir, "push", "origin", "HEAD:main")
	return dir
}

// groupClaude fixes app.txt in the current directory and, when LIB=1, lib.txt in every
// --add-dir directory (the other worktrees of the group).
const groupClaude = `echo fixed > app.txt
grab=0
for a in "$@"; do
  case "$a" in
    --add-dir) grab=1 ;;
    --*) grab=0 ;;
    *) if [ $grab = 1 ] && [ "$LIB" = 1 ]; then echo fixed > "$a/lib.txt"; fi ;;
  esac
done
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"### Root cause\nbug","total_cost_usd":0.2,"num_turns":4}'
`

type scopeJira struct{ body string }

func (s scopeJira) GetIssue(ctx context.Context, key string) (jira.IssueDetail, error) {
	d, _ := fakeJira{}.GetIssue(ctx, key)
	d.Comments = []jira.Comment{{Author: "Hung", Body: s.body}}
	return d, nil
}

func groupWorker(t *testing.T) (*Worker, *fakeTG, string, string) {
	w, tg, clone, _ := setup(t, groupClaude)
	lib := addRepo(t, clone, "lib")
	w.Repos["X"] = append(w.Repos["X"], repos.Repo{Name: "lib", GitLabProject: "g/lib", Path: lib, BaseBranch: "main",
		TestCmd: "grep -q fixed lib.txt"})
	w.Jira = scopeJira{body: "@CLAUDE/CODEX\nrepo_dependencies: app, lib\nhints: lib.txt"}
	w.NoPush = true
	ctx := context.Background()
	// Replace setup's single-repo job by a two-repo group on another task.
	w.Store.Upsert(ctx, store.IssueInput{Key: "X-2", Summary: "Cross-service", URL: "u"}, w.Now())
	w.Store.MarkNotified(ctx, "X-2", 8, w.Now())
	if _, _, err := w.Store.ClaimGroup(ctx, w.Now()); err != nil { // drop X-1's job
		t.Fatal(err)
	}
	if _, _, err := w.Store.ApproveAndEnqueueRepos(ctx, "X-2", []string{"app", "lib"}, w.Now()); err != nil {
		t.Fatal(err)
	}
	return w, tg, clone, lib
}

func TestGroupOneAgentAllRepos(t *testing.T) {
	ctx := context.Background()
	t.Setenv("LIB", "1")
	w, tg, clone, lib := groupWorker(t)

	if did, err := w.RunOnce(ctx); err != nil || !did {
		t.Fatalf("RunOnce = %v %v", did, err)
	}
	jobs, _ := w.Store.LatestGroup(ctx, "X-2")
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	for i, dir := range []string{clone, lib} {
		j := jobs[i]
		if j.Status != store.JobSucceeded || j.TestsPassed == nil || !*j.TestsPassed {
			t.Fatalf("job %s = %+v", j.RepoName, j)
		}
		if msg := git(t, dir, "log", "-1", "--format=%s", j.Branch); !strings.HasPrefix(msg, "fix(X-2)") {
			t.Errorf("%s commit = %q", j.RepoName, msg)
		}
		if _, err := os.Stat(filepath.Join(w.JobsDir, strconv.FormatInt(j.ID, 10), "agent.json")); err != nil {
			t.Errorf("%s agent log missing: %v", j.RepoName, err)
		}
	}
	last := tg.edits[len(tg.edits)-1]
	if !strings.Contains(last, "<code>app</code>") || !strings.Contains(last, "<code>lib</code>") {
		t.Errorf("group footer = %s", last)
	}
	if !strings.Contains(tg.edits[0], "Agent running</b> on <code>app, lib</code>") {
		t.Errorf("running footer = %s", tg.edits[0])
	}
}

func TestGroupRepoWithoutChangesIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	w, tg, _, _ := groupWorker(t) // LIB unset: only app.txt changes

	w.RunOnce(ctx)
	jobs, _ := w.Store.LatestGroup(ctx, "X-2")
	if jobs[0].Status != store.JobSucceeded || jobs[0].Error != "" {
		t.Fatalf("app job = %+v", jobs[0])
	}
	if jobs[1].Status != store.JobSucceeded || jobs[1].Error != "no changes needed in this repo" {
		t.Fatalf("lib job = %+v", jobs[1])
	}
	if last := tg.edits[len(tg.edits)-1]; !strings.Contains(last, "lib</code>: no changes needed") {
		t.Errorf("footer = %s", last)
	}
	reports := 0
	for _, s := range tg.sends {
		if strings.Contains(s, "📋") {
			reports++
		}
	}
	if reports != 1 {
		t.Errorf("want one report (app only), got %d: %v", reports, tg.sends)
	}
}
