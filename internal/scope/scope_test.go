package scope

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
)

var known = []string{"cd-host-suite", "cd-boclient-web", "cd-api"}

func TestExampleMatchesDefault(t *testing.T) {
	f, err := Load("../../scope.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f, Default()) {
		t.Error("scope.example.json drifted from the built-in default.json")
	}
	if m, _ := Load(filepath.Join(t.TempDir(), "missing.json")); m.Marker != "@CLAUDE/CODEX" {
		t.Errorf("missing file should give the default, got %+v", m)
	}
}

func TestLoadValidates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "scope.json")
	os.WriteFile(p, []byte(`{"marker":"","fields":[{"key":"a b"},{"key":"x","role":"nope"}]}`), 0o644)
	_, err := Load(p)
	for _, want := range []string{"marker is required", `key "a b"`, `unknown role "nope"`, "exactly one field"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestFindParsesUserFormat(t *testing.T) {
	body := "---\n@CLAUDE/CODEX,\nrepo_dependencies: cd-host-suite, cd-boclient-web\nspecs: Thêm cột Ngày duyệt\n  vào màn hình danh sách\nhints: xem ApprovalService.cs\nNote: dữ liệu ở bảng X\n---"
	comments := []jira.Comment{
		{Author: "QA", Body: "steps to reproduce"},
		{Author: "Hung", AuthorName: "hungnd", Body: body},
		{Author: "QA", Body: "any update?"},
	}
	r := Default().Find(comments, known)
	if r.Scope == nil || len(r.Errors) > 0 {
		t.Fatalf("result = %+v", r)
	}
	s := r.Scope
	if !reflect.DeepEqual(s.Repos, []string{"cd-host-suite", "cd-boclient-web"}) || s.Author != "Hung" {
		t.Errorf("scope = %+v", s)
	}
	if len(s.Notes) != 2 || s.Notes[0].Key != "specs" || !strings.Contains(s.Notes[0].Value, "vào màn hình") ||
		!strings.Contains(s.Notes[1].Value, "Note: dữ liệu") {
		t.Errorf("notes = %+v", s.Notes)
	}
}

func TestFindLatestAndErrors(t *testing.T) {
	f := Default()
	if r := f.Find([]jira.Comment{{Body: "please fix"}}, known); r.Found || r.Scope != nil {
		t.Errorf("no marker: %+v", r)
	}
	// The untouched template is rejected: placeholders count as empty.
	tpl := f.Template(nil)
	if r := f.Find([]jira.Comment{{Body: tpl}}, known); r.Scope != nil || !r.Found || !strings.Contains(strings.Join(r.Errors, ";"), "thiếu repo_dependencies") {
		t.Errorf("template as is: %+v", r)
	}
	bad := "@claude/codex\nrepo_dependencies: cd-api, etc"
	r := f.Find([]jira.Comment{{Body: bad}}, known)
	if r.Scope != nil || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "etc") || !strings.Contains(r.Errors[0], "cd-host-suite") {
		t.Errorf("unknown repo: %+v", r)
	}
	// The latest marked comment wins (an older valid one does not rescue a newer invalid one).
	good := "@CLAUDE/CODEX, repo_dependencies: cd-api"
	if r := f.Find([]jira.Comment{{Body: bad}, {Body: good}}, known); r.Scope == nil || r.Scope.Repos[0] != "cd-api" {
		t.Errorf("latest good: %+v", r)
	}
	if r := f.Find([]jira.Comment{{Body: good}, {Body: bad}}, known); r.Scope != nil {
		t.Errorf("latest bad should win: %+v", r)
	}
}

func TestAuthors(t *testing.T) {
	f := Default()
	f.Authors = []string{"hungnd"}
	c := []jira.Comment{{Author: "Hung", AuthorName: "hungnd", Body: "@CLAUDE/CODEX\nrepo_dependencies: cd-api"},
		{Author: "Someone", AuthorName: "x", Body: "@CLAUDE/CODEX\nrepo_dependencies: cd-host-suite"}}
	if r := f.Find(c, known); r.Scope == nil || r.Scope.Repos[0] != "cd-api" {
		t.Errorf("authors filter: %+v", r)
	}
}

func TestTemplateRoundTrip(t *testing.T) {
	f := Default()
	tpl := f.Template(known)
	if !strings.HasPrefix(tpl, "@CLAUDE/CODEX,\nrepo_dependencies: cd-host-suite, cd-boclient-web, cd-api\nspecs: <") {
		t.Errorf("template = %q", tpl)
	}
	r := f.Find([]jira.Comment{{Body: tpl}}, known)
	if r.Scope == nil || len(r.Scope.Repos) != 3 || len(r.Scope.Notes) != 0 {
		t.Errorf("prefilled template should be valid with no notes: %+v", r)
	}
	if g := f.Guide(); len(g) != 3 || !strings.Contains(g[0], "bắt buộc") {
		t.Errorf("guide = %v", g)
	}
}
