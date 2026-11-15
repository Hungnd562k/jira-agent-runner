package scheduler

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/Hungnd562k/jira-agent-runner/internal/scope"
	"github.com/Hungnd562k/jira-agent-runner/internal/store"
)

// scopeCheck is the maintainer's scope comment as read on Fix it / Retry.
type scopeCheck struct {
	configured bool         // repos.json lists repos for the issue's project
	candidates []string     // those repo names
	scope      *scope.Scope // nil = no valid scope comment
	errors     []string     // why the latest scope comment was rejected
	url        string       // issue link, for the guide
	err        error        // Jira could not be read
}

// checkScope reads the issue's comments and finds the scope comment. Projects without
// configured repos need no scope (the task is only approved).
func (s *Service) checkScope(ctx context.Context, key string) scopeCheck {
	var sc scopeCheck
	for _, r := range s.Repos.For(key) {
		sc.candidates = append(sc.candidates, r.Name)
	}
	if sc.configured = len(sc.candidates) > 0; !sc.configured {
		return sc
	}
	if s.Issues == nil {
		sc.err = errors.New("no Jira client configured")
		return sc
	}
	d, err := s.Issues.GetIssue(ctx, key)
	if err != nil {
		sc.err = err
		return sc
	}
	sc.url = d.URL
	r := s.Scope.Find(d.Comments, sc.candidates)
	sc.scope, sc.errors = r.Scope, r.Errors
	return sc
}

// askScope replies to the notice with the copy-paste scope template; the task is unchanged.
func (s *Service) askScope(ctx context.Context, t store.Task, msgID int64, sc scopeCheck) {
	text := ScopeGuide(s.Scope, t.JiraKey, firstNonEmpty(sc.url, t.URL), sc.candidates, sc.errors)
	if _, err := s.TG.SendMessage(ctx, s.ChatID, text, nil, msgID); err != nil {
		s.Log.Warn("send scope guide", "key", t.JiraKey, "err", err)
	}
	s.Log.Info("scope comment missing or invalid", "key", t.JiraKey, "errors", strings.Join(sc.errors, "; "))
}

// ScopeGuide renders the "maintainer must set the scope" message: why, the template to paste
// on the ticket (a <pre> block is tap-to-copy in Telegram) and what each field means.
func ScopeGuide(f scope.Format, key, url string, candidates, problems []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📝 <b>[%s] cần phạm vi sửa từ maintainer</b>\n", html.EscapeString(key))
	if len(problems) > 0 {
		b.WriteString("Comment phạm vi mới nhất trên ticket chưa hợp lệ:\n")
		for _, p := range problems {
			b.WriteString("• " + html.EscapeString(p) + "\n")
		}
	} else {
		fmt.Fprintf(&b, "Ticket chưa có comment bắt đầu bằng <code>%s</code>, nên agent chưa biết được sửa ở repo nào.\n",
			html.EscapeString(f.Marker))
	}
	if len(f.Authors) > 0 {
		fmt.Fprintf(&b, "Chỉ nhận comment của: %s\n", html.EscapeString(strings.Join(f.Authors, ", ")))
	}
	fmt.Fprintf(&b, "\nCopy mẫu dưới, dán thành comment trên <a href=\"%s\">%s</a>, xoá repo không liên quan, điền specs/hints rồi bấm 🛠 lại:\n",
		html.EscapeString(url), html.EscapeString(key))
	b.WriteString("<pre>" + html.EscapeString(f.Template(candidates)) + "</pre>\n")
	b.WriteString("<b>Các trường</b>\n")
	for _, g := range f.Guide() {
		b.WriteString("• " + html.EscapeString(g) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// retryRepos is the scope minus repos whose fix is already merged in the latest group.
func (s *Service) retryRepos(ctx context.Context, key string, scoped []string) []string {
	merged := map[string]bool{}
	if g, err := s.Store.LatestGroup(ctx, key); err == nil {
		for _, j := range g {
			if !j.MergedAt.IsZero() {
				merged[j.RepoName] = true
			}
		}
	}
	var out []string
	for _, r := range scoped {
		if !merged[r] {
			out = append(out, r)
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
