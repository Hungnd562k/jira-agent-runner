package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/issue/X-1" || r.Header.Get("Authorization") != "Bearer pat" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var comments []string
		for i := range 7 {
			comments = append(comments, fmt.Sprintf(`{"body":"c%d","created":"2026-11-10T10:00:00.000+0700","author":{"displayName":"Dev %d"}}`, i, i))
		}
		fmt.Fprintf(w, `{"key":"X-1","fields":{"summary":"Crash on save","description":"Steps: 1. open 2. save",
			"updated":"2026-11-10T10:00:00.000+0700","labels":["backend"],"issuetype":{"name":"Bug"},
			"priority":{"name":"High"},"status":{"name":"To Do"},"components":[{"name":"API"}],
			"comment":{"comments":[%s]}}}`, strings.Join(comments, ","))
	}))
	defer srv.Close()

	d, err := New(srv.URL, "pat").GetIssue(context.Background(), "X-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary != "Crash on save" || d.Type != "Bug" || d.Priority != "High" || d.Components[0] != "API" ||
		!strings.Contains(d.Description, "Steps") || d.URL != srv.URL+"/browse/X-1" {
		t.Errorf("detail = %+v", d)
	}
	if len(d.Comments) != maxComments || d.Comments[0].Body != "c2" || d.Comments[4].Author != "Dev 6" {
		t.Errorf("comments = %+v", d.Comments)
	}

	if _, err := New(srv.URL, "wrong").GetIssue(context.Background(), "X-1"); err == nil {
		t.Error("expected error")
	}
}

func TestAddComment(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rest/api/2/issue/X-1/comment" || r.Header.Get("Authorization") != "Bearer pat" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()

	if err := New(srv.URL, "pat").AddComment(context.Background(), "X-1", "hello"); err != nil || body["body"] != "hello" {
		t.Fatalf("AddComment err=%v body=%v", err, body)
	}
	if err := New(srv.URL, "bad").AddComment(context.Background(), "X-1", "x"); err == nil {
		t.Error("expected error")
	}
}

func TestAddAttachment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/issue/X-1/attachments" || r.Header.Get("X-Atlassian-Token") != "no-check" ||
			r.Header.Get("Authorization") != "Bearer pat" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		fmt.Fprintf(w, `[{"filename":%q,"size":%d}]`, h.Filename, h.Size)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "01-shot.png")
	os.WriteFile(path, []byte("png"), 0o600)

	got, err := New(srv.URL, "pat").AddAttachment(context.Background(), "X-1", path, "X-1-j2-01-shot.png")
	if err != nil || got != "X-1-j2-01-shot.png" {
		t.Fatalf("AddAttachment = %q %v", got, err)
	}
	if _, err := New(srv.URL, "bad").AddAttachment(context.Background(), "X-1", path, ""); err == nil {
		t.Error("expected error on HTTP 400")
	}
}

func TestTransitionAndAssign(t *testing.T) {
	var posted, assigned map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/2/issue/X-1" && r.URL.Query().Get("fields") == "status":
			fmt.Fprint(w, `{"fields":{"status":{"name":"In Progress"}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/2/issue/X-1/transitions":
			fmt.Fprint(w, `{"transitions":[{"id":"11","name":"Start","to":{"name":"In Progress"}},
				{"id":"5","name":"Resolve Issue","to":{"name":"Resolved"},"fields":{"resolution":{"required":true}}}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/2/issue/X-1/transitions":
			json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Query().Get("fields") == "reporter":
			fmt.Fprint(w, `{"fields":{"reporter":{"name":"qc.an","displayName":"QC An"}}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/rest/api/2/issue/X-1/assignee":
			json.NewDecoder(r.Body).Decode(&assigned)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "pat")
	ctx := context.Background()

	to, err := c.Transition(ctx, "X-1", "resolved", "Fixed")
	if err != nil || to != "Resolved" {
		t.Fatalf("Transition = %q %v", to, err)
	}
	if posted["transition"].(map[string]any)["id"] != "5" || posted["fields"].(map[string]any)["resolution"].(map[string]any)["name"] != "Fixed" {
		t.Errorf("transition payload = %v", posted)
	}
	if _, err := c.Transition(ctx, "X-1", "Closed", ""); err == nil || !strings.Contains(err.Error(), "Resolve Issue→Resolved") {
		t.Errorf("missing transition error = %v", err)
	}
	name, err := c.AssignToReporter(ctx, "X-1")
	if err != nil || name != "QC An" || assigned["name"] != "qc.an" {
		t.Fatalf("AssignToReporter = %q %v %v", name, err, assigned)
	}
}
