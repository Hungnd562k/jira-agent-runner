package repos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "repos.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndLookup(t *testing.T) {
	c, err := Load(write(t, `{"HDBCD":[
		{"name":"api","gitlab_project":"g/api","path":"/src/api","base_branch":"develop","test_cmd":"go test ./..."},
		{"name":"web","gitlab_project":"g/web","path":"/src/web","base_branch":"main"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.For("HDBCD-2700"); len(got) != 2 {
		t.Fatalf("For = %+v", got)
	}
	if r, ok := c.Find("HDBCD-1", "web"); !ok || r.GitLabProject != "g/web" {
		t.Fatalf("Find = %+v %v", r, ok)
	}
	if got := c.For("OTHER-1"); len(got) != 0 {
		t.Fatalf("unrelated project: %+v", got)
	}
	if ProjectOf("MY-PROJ-12") != "MY-PROJ" {
		t.Errorf("ProjectOf multi-dash = %q", ProjectOf("MY-PROJ-12"))
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(c) != 0 {
		t.Fatalf("Load missing = %v %v", c, err)
	}
}

func TestLoadValidation(t *testing.T) {
	_, err := Load(write(t, `{"P":[
		{"name":"a","gitlab_project":"g/a","path":"relative","base_branch":"main"},
		{"name":"a","gitlab_project":"","path":"/x","base_branch":""}]}`))
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"absolute", "duplicate", "gitlab_project", "base_branch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if _, err := Load(write(t, `{"P":[{"name":"a","typo":1}]}`)); err == nil {
		t.Error("unknown field should fail")
	}
}

func TestNormalizeProject(t *testing.T) {
	cases := map[string]string{
		"git@192.168.2.181:dsb/DSBNova/cd-host-job.git":          "dsb/DSBNova/cd-host-job",
		"ssh://git@gitlab.example.com:2222/group/app.git":        "group/app",
		"https://devx.navisoft.com.vn/dsb/DSBNova/cd-x-host.git": "dsb/DSBNova/cd-x-host",
		"group/sub/app":  "group/sub/app",
		"/group/app.git": "group/app",
	}
	for in, want := range cases {
		if got := NormalizeProject(in); got != want {
			t.Errorf("NormalizeProject(%q) = %q, want %q", in, got, want)
		}
	}
}
