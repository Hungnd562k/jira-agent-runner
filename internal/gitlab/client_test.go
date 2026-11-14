package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Hungnd562k/jira-agent-runner/internal/repos"
	"github.com/Hungnd562k/jira-agent-runner/internal/worker"
)

func TestOpenDraftMR(t *testing.T) {
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
	}
	url, err := New(srv.URL+"/", "tok").OpenDraftMR(context.Background(), req)
	if err != nil || url != "https://gl/group/sub/api/-/merge_requests/7" {
		t.Fatalf("OpenDraftMR = %q %v", url, err)
	}
	if got.Title != "Draft: [X-1] fix" || got.TargetBranch != "develop" || got.SourceBranch != "agent/X-1-j1" || !got.RemoveSourceBranch {
		t.Errorf("payload = %+v", got)
	}

	if _, err := New(srv.URL, "bad").OpenDraftMR(context.Background(), req); err == nil {
		t.Error("expected error on 404")
	}
}
