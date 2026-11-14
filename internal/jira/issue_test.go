package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
