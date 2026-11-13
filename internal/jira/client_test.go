package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestSearchPaginatesAndMaps(t *testing.T) {
	const total = 3
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("auth header = %q", got)
		}
		if r.URL.Path != "/rest/api/2/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("jql") != "project = X" {
			t.Errorf("jql = %q", r.URL.Query().Get("jql"))
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		end := min(start+2, total)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"startAt":%d,"maxResults":2,"total":%d,"issues":[`, start, total)
		for i := start; i < end; i++ {
			if i > start {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"key":"X-%d","fields":{"summary":"bug %d","updated":"2026-11-10T10:00:00.000+0700","priority":{"name":"High"},"status":{"name":"Open"}}}`, i, i)
		}
		fmt.Fprint(w, `]}`)
	}))
	defer srv.Close()

	c := New(srv.URL+"/", "secret", WithPageSize(2))
	issues, err := c.Search(context.Background(), "project = X")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 page calls, got %d", calls)
	}
	if len(issues) != total {
		t.Fatalf("got %d issues", len(issues))
	}
	got := issues[1]
	if got.Key != "X-1" || got.Summary != "bug 1" || got.Priority != "High" || got.Status != "Open" {
		t.Errorf("mapping wrong: %+v", got)
	}
	if got.URL != srv.URL+"/browse/X-1" {
		t.Errorf("url = %q", got.URL)
	}
	if got.Updated.IsZero() || got.Updated.UTC().Hour() != 3 {
		t.Errorf("updated parsed wrong: %v", got.Updated)
	}
}

func TestSearchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad jql", http.StatusBadRequest)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "x").Search(context.Background(), "???"); err == nil {
		t.Fatal("expected error")
	}
}
