// Package scope reads the maintainer's "scope comment" on a Jira issue: which repos the agent
// may change, plus free-form guidance (specs, hints). The comment format lives in scope.json so
// it can be changed without touching code; see scope.example.json.
package scope

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/Hungnd562k/jira-agent-runner/internal/jira"
)

// RoleRepos marks the field that lists the repos in scope (names from repos.json).
const RoleRepos = "repos"

// Field is one "key: value" line of the scope comment.
type Field struct {
	Key         string `json:"key"`                   // e.g. "repo_dependencies" (matched case-insensitively)
	Role        string `json:"role,omitempty"`        // "repos" for the repo list; empty = guidance for the agent
	Required    bool   `json:"required,omitempty"`    // comment is rejected when the value is empty
	Placeholder string `json:"placeholder,omitempty"` // value shown in the template; left as is = empty
	Description string `json:"description,omitempty"` // what the field means (template guide + agent prompt)
}

// Format describes the scope comment. Edit scope.json to change it.
type Format struct {
	// Marker is the first line of a scope comment, e.g. "@CLAUDE/CODEX" (case-insensitive;
	// trailing , : ; and *_ wiki formatting are ignored).
	Marker string `json:"marker"`
	// Authors, when non-empty, accepts scope comments only from these Jira users
	// (username or display name, case-insensitive). Empty = any author.
	Authors []string `json:"authors,omitempty"`
	Fields  []Field  `json:"fields"`
}

//go:embed default.json
var defaultJSON []byte

// Default is the built-in format, used when scope.json does not exist.
func Default() Format {
	f, err := parse(defaultJSON)
	if err != nil {
		panic(err) // embedded at build time and covered by tests
	}
	return f
}

// Load reads and validates path. A missing file yields Default().
func Load(path string) (Format, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Format{}, err
	}
	f, err := parse(raw)
	if err != nil {
		return Format{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func parse(raw []byte) (Format, error) {
	var f Format
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Format{}, err
	}
	return f, f.validate()
}

func (f Format) validate() error {
	var errs []error
	if strings.TrimSpace(f.Marker) == "" {
		errs = append(errs, errors.New("marker is required"))
	}
	seen := map[string]bool{}
	repos := 0
	for i, fd := range f.Fields {
		k := strings.ToLower(strings.TrimSpace(fd.Key))
		switch {
		case !keyRe.MatchString(k):
			errs = append(errs, fmt.Errorf("fields[%d]: key %q must be letters, digits, _ or -", i, fd.Key))
		case seen[k]:
			errs = append(errs, fmt.Errorf("fields[%d]: duplicate key %q", i, fd.Key))
		}
		seen[k] = true
		switch fd.Role {
		case RoleRepos:
			repos++
		case "":
		default:
			errs = append(errs, fmt.Errorf("fields[%d]: unknown role %q (only %q)", i, fd.Role, RoleRepos))
		}
	}
	if repos != 1 {
		errs = append(errs, fmt.Errorf("exactly one field must have role %q (found %d)", RoleRepos, repos))
	}
	return errors.Join(errs...)
}

var keyRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

func (f Format) reposField() Field {
	for _, fd := range f.Fields {
		if fd.Role == RoleRepos {
			return fd
		}
	}
	return Field{}
}

// Note is one guidance field taken from the comment (specs, hints…), in format order.
type Note struct {
	Key, Description, Value string
}

// Scope is a parsed, valid scope comment.
type Scope struct {
	Repos   []string // repo names, in the order the maintainer listed them
	Notes   []Note   // non-empty guidance fields
	Author  string
	Comment string // the raw comment body
}

// Result of looking for the scope comment on an issue.
type Result struct {
	Scope  *Scope   // nil = no valid scope comment
	Found  bool     // a comment with the marker exists (it may be invalid)
	Errors []string // why the latest marked comment was rejected
}

// Find parses the latest comment that starts with the marker. known are the repo names
// configured for the issue's project; repos outside it are rejected.
func (f Format) Find(comments []jira.Comment, known []string) Result {
	for i := len(comments) - 1; i >= 0; i-- {
		c := comments[i]
		body, ok := f.afterMarker(c.Body)
		if !ok {
			continue
		}
		if !f.authorAllowed(c) {
			continue // a marker from someone else is ignored, not an error
		}
		s, errs := f.parseBody(body, known)
		if len(errs) > 0 {
			return Result{Found: true, Errors: errs}
		}
		s.Author, s.Comment = c.Author, c.Body
		return Result{Scope: s, Found: true}
	}
	return Result{}
}

func (f Format) authorAllowed(c jira.Comment) bool {
	if len(f.Authors) == 0 {
		return true
	}
	for _, a := range f.Authors {
		if strings.EqualFold(a, c.AuthorName) || strings.EqualFold(a, c.Author) {
			return true
		}
	}
	return false
}

// afterMarker returns the comment text after the marker line, when the first meaningful line
// is the marker (text on the same line after the marker is kept).
func (f Format) afterMarker(body string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	marker := strings.ToLower(strings.TrimSpace(f.Marker))
	for i, l := range lines {
		t := strings.Trim(strings.TrimSpace(l), "*_")
		if t == "" || isRule(t) {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(t), marker) {
			return "", false
		}
		rest := strings.TrimLeft(t[len(marker):], ",:; \t*_")
		return strings.Join(append([]string{rest}, lines[i+1:]...), "\n"), true
	}
	return "", false
}

// isRule reports a separator line like --- or ----.
func isRule(s string) bool { return strings.Trim(s, "-—– ") == "" }

func (f Format) parseBody(body string, known []string) (*Scope, []string) {
	byKey := map[string]int{}
	for i, fd := range f.Fields {
		byKey[strings.ToLower(fd.Key)] = i
	}
	values := make([][]string, len(f.Fields))
	cur := -1
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if isRule(t) && t != "" {
			continue
		}
		if m := lineRe.FindStringSubmatch(t); m != nil {
			if i, ok := byKey[strings.ToLower(m[1])]; ok {
				cur = i
				values[i] = append(values[i], m[2])
				continue
			}
		}
		if cur >= 0 {
			values[cur] = append(values[cur], strings.TrimRight(l, " \t"))
		}
	}

	var errs []string
	s := &Scope{}
	for i, fd := range f.Fields {
		v := cleanValue(strings.Join(values[i], "\n"), fd.Placeholder)
		if v == "" {
			if fd.Required {
				errs = append(errs, fmt.Sprintf("thiếu %s", fd.Key))
			}
			continue
		}
		if fd.Role == RoleRepos {
			s.Repos, errs = splitRepos(v, known, fd.Key, errs)
			continue
		}
		s.Notes = append(s.Notes, Note{Key: fd.Key, Description: fd.Description, Value: v})
	}
	if len(errs) == 0 && len(s.Repos) == 0 {
		errs = append(errs, fmt.Sprintf("%s không có repo nào", f.reposField().Key))
	}
	return s, errs
}

// lineRe matches "key: value" (also "- key = value", "*key*: value").
var lineRe = regexp.MustCompile(`^[-*•]?\s*\*?([A-Za-z0-9_-]+)\*?\s*[:=]\s*(.*)$`)

// cleanValue trims a field value; an untouched placeholder or a lone <…> hint counts as empty.
func cleanValue(v, placeholder string) string {
	v = strings.TrimSpace(v)
	if v == "" || (placeholder != "" && v == strings.TrimSpace(placeholder)) {
		return ""
	}
	if strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") && !strings.Contains(v, "\n") {
		return ""
	}
	return v
}

func splitRepos(v string, known []string, key string, errs []string) ([]string, []string) {
	var out []string
	var unknown []string
	for _, n := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == ' ' || r == '\t' }) {
		n = strings.Trim(n, "{}*_`")
		if n == "" || slices.Contains(out, n) {
			continue
		}
		if !slices.Contains(known, n) {
			unknown = append(unknown, n)
			continue
		}
		out = append(out, n)
	}
	if len(unknown) > 0 {
		valid := strings.Join(known, ", ")
		if valid == "" {
			valid = "(chưa cấu hình repo nào cho project này trong repos.json)"
		}
		errs = append(errs, fmt.Sprintf("%s có repo không hợp lệ: %s — repo hợp lệ: %s", key, strings.Join(unknown, ", "), valid))
	}
	return out, errs
}

// Template is the comment the maintainer copies onto the ticket. The repos field is prefilled
// with every candidate repo (delete the ones not involved).
func (f Format) Template(candidates []string) string {
	var b strings.Builder
	b.WriteString(f.Marker + ",\n")
	for _, fd := range f.Fields {
		v := fd.Placeholder
		if fd.Role == RoleRepos && len(candidates) > 0 {
			v = strings.Join(candidates, ", ")
		}
		fmt.Fprintf(&b, "%s: %s\n", fd.Key, v)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Guide lists the fields with their meaning, for the Telegram message.
func (f Format) Guide() []string {
	out := make([]string, 0, len(f.Fields))
	for _, fd := range f.Fields {
		line := fd.Key
		if fd.Required {
			line += " (bắt buộc)"
		}
		if fd.Description != "" {
			line += ": " + fd.Description
		}
		out = append(out, line)
	}
	return out
}
