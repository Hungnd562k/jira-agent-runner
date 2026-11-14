package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/worker"
)

func TestOpenMR(t *testing.T) {
	var got mrCreate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v4/projects/group%2Fsub%2Fapi/merge_requests" || r.Header.Get("PRIVATE-TOKEN") != "tok" {
			http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"iid":7,"web_url":"https://gl/group/sub/api/-/merge_requests/7"}`))
	}))
	defer srv.Close()

	req := worker.MRRequest{
		Repo:         repos.Repo{GitLabProject: "group/sub/api", BaseBranch: "develop"},
		SourceBranch: "agent/X-1-j1",
		Title:        "[X-1] fix",
		Description:  "desc",
		Draft:        true,
	}
	mr, err := New(srv.URL+"/", "tok").OpenMR(context.Background(), req)
	if err != nil || mr.IID != 7 || mr.WebURL != "https://gl/group/sub/api/-/merge_requests/7" {
		t.Fatalf("OpenMR = %+v %v", mr, err)
	}
	if got.Title != "Draft: [X-1] fix" || got.TargetBranch != "develop" || got.SourceBranch != "agent/X-1-j1" || !got.RemoveSourceBranch {
		t.Errorf("payload = %+v", got)
	}

	// Handover MR: explicit target, not draft, source branch kept.
	req.Draft, req.Target, req.KeepSource, req.SourceBranch = false, "test", true, "develop"
	if _, err := New(srv.URL, "tok").OpenMR(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got.Title != "[X-1] fix" || got.TargetBranch != "test" || got.RemoveSourceBranch {
		t.Errorf("handover payload = %+v", got)
	}
	if _, err := New(srv.URL, "bad").OpenMR(context.Background(), req); err == nil {
		t.Error("expected error on 404")
	}
}

func TestMergeMRWaitsForMergeability(t *testing.T) {
	gets, merged := 0, map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/merge_requests/5"):
			gets++
			status := "checking"
			if gets > 2 {
				status = "mergeable"
			}
			fmt.Fprintf(w, `{"iid":5,"state":"opened","detailed_merge_status":%q}`, status)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge_requests/5/merge"):
			json.NewDecoder(r.Body).Decode(&merged)
			fmt.Fprint(w, `{"iid":5,"state":"merged","merge_commit_sha":"abc123"}`)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	c.mergeWait = 0
	sha, err := c.MergeMR(context.Background(), "g/app", 5, false)
	if err != nil || sha != "abc123" || gets != 3 || merged["should_remove_source_branch"] != false {
		t.Fatalf("MergeMR = %q %v gets=%d body=%v", sha, err, gets, merged)
	}
}

func TestMergeMRConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"iid":5,"state":"opened","detailed_merge_status":"broken_status","has_conflicts":true}`)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "tok").MergeMR(context.Background(), "g/app", 5, true); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestPipelinesAndJobLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pipelines") && r.URL.Query().Get("sha") == "abc" && r.URL.Query().Get("ref") == "test":
			fmt.Fprint(w, `[{"id":42,"status":"running","web_url":"https://gl/p/42"}]`)
		case strings.HasSuffix(r.URL.Path, "/pipelines"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/pipelines/42"):
			fmt.Fprint(w, `{"id":42,"status":"failed","web_url":"https://gl/p/42"}`)
		case strings.HasSuffix(r.URL.Path, "/pipelines/42/jobs"):
			fmt.Fprint(w, `[{"id":9,"name":"build","stage":"build","web_url":"https://gl/j/9","failure_reason":"script_failure"}]`)
		case strings.HasSuffix(r.URL.Path, "/jobs/9/trace"):
			fmt.Fprint(w, "section_start:1:step\r\x1b[32;1mRunning\x1b[0m\nerror CS1002: ; expected\n\nERROR: Job failed: exit code 1\n")
		default:
			http.Error(w, "unexpected "+r.URL.String(), http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c, ctx := New(srv.URL, "tok"), context.Background()

	p, ok, err := c.FindPipeline(ctx, "g/app", "test", "abc")
	if err != nil || !ok || p.ID != 42 {
		t.Fatalf("FindPipeline = %+v %v %v", p, ok, err)
	}
	if _, ok, _ := c.FindPipeline(ctx, "g/app", "test", "zzz"); ok {
		t.Error("no pipeline expected for another sha")
	}
	if p, _ = c.GetPipeline(ctx, "g/app", 42); p.Status != "failed" {
		t.Errorf("GetPipeline = %+v", p)
	}
	jobs, err := c.FailedJobs(ctx, "g/app", 42)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "build" || jobs[0].FailureReason != "script_failure" {
		t.Fatalf("FailedJobs = %+v %v", jobs, err)
	}
	tail, err := c.JobLogTail(ctx, "g/app", 9, 2)
	if err != nil || tail != "error CS1002: ; expected\nERROR: Job failed: exit code 1" {
		t.Fatalf("JobLogTail = %q %v", tail, err)
	}
}
