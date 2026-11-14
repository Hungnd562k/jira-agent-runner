package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scenario is one browser check the agent recorded in e2e.json.
type Scenario struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Steps       string   `json:"steps"`
	Expected    string   `json:"expected"`
	Result      string   `json:"result"` // PASS | FAIL | BLOCKED
	Screenshots []string `json:"screenshots"`
}

// Evidence is the E2E outcome the agent left in <job dir>/evidence.
type Evidence struct {
	Environment string     `json:"environment"`
	Scenarios   []Scenario `json:"scenarios"`
	Blocked     string     `json:"blocked"`

	Dir      string            `json:"-"`
	Shots    []string          `json:"-"` // *.png present in Dir, sorted
	Problem  string            `json:"-"` // e2e.json missing or invalid
	Uploaded map[string]string `json:"-"` // shot → Jira attachment name
}

// loadEvidence reads dir/e2e.json and the screenshots next to it. Never fails: problems are
// recorded so the report shows the verification as incomplete instead of hiding it.
func loadEvidence(dir string) *Evidence {
	e := &Evidence{Dir: dir}
	shots, _ := filepath.Glob(filepath.Join(dir, "*.png"))
	for _, s := range shots {
		e.Shots = append(e.Shots, filepath.Base(s))
	}
	sort.Strings(e.Shots)
	raw, err := os.ReadFile(filepath.Join(dir, "e2e.json"))
	switch {
	case err != nil:
		e.Problem = "agent wrote no e2e.json"
	case json.Unmarshal(raw, e) != nil:
		e.Problem = "e2e.json is not valid JSON"
	case len(e.Scenarios) == 0 && e.Blocked == "":
		e.Problem = "e2e.json lists no scenarios"
	}
	return e
}

func (e *Evidence) counts() (pass, fail, other int) {
	for _, s := range e.Scenarios {
		switch strings.ToUpper(strings.TrimSpace(s.Result)) {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		default:
			other++
		}
	}
	return
}

// Complete is true only when every scenario passed and screenshots exist.
func (e *Evidence) Complete() bool {
	pass, fail, other := e.counts()
	return e.Problem == "" && e.Blocked == "" && fail == 0 && other == 0 && pass > 0 && len(e.Shots) > 0
}

// Line is the one-line E2E status used in Telegram, report.md, the MR and Jira.
func (e *Evidence) Line() string {
	pass, fail, _ := e.counts()
	total := len(e.Scenarios)
	switch {
	case e.Problem != "":
		return "⚠️ incomplete — " + e.Problem
	case e.Blocked != "":
		return fmt.Sprintf("⚠️ BLOCKED (%d/%d PASS) — %s", pass, total, truncate(e.Blocked, 200))
	case fail > 0:
		return fmt.Sprintf("❌ %d/%d FAIL · %d screenshots", fail, total, len(e.Shots))
	case len(e.Shots) == 0:
		return fmt.Sprintf("⚠️ %d/%d PASS but no screenshots — incomplete", pass, total)
	case !e.Complete():
		return fmt.Sprintf("⚠️ %d/%d PASS · %d screenshots", pass, total, len(e.Shots))
	default:
		return fmt.Sprintf("✅ %d/%d PASS · %d screenshots", pass, total, len(e.Shots))
	}
}

// Markdown renders the scenario table (report.md and the MR description).
func (e *Evidence) Markdown() string {
	var b strings.Builder
	b.WriteString("**E2E:** " + e.Line() + "\n\n")
	if e.Environment != "" {
		b.WriteString("Environment: " + e.Environment + "\n\n")
	}
	if len(e.Scenarios) > 0 {
		b.WriteString("| # | Scenario | Expected | Result | Screenshots |\n|---|---|---|---|---|\n")
		for _, s := range e.Scenarios {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", mdCell(s.ID), mdCell(s.Name), mdCell(s.Expected),
				mdCell(s.Result), mdCell(strings.Join(s.Screenshots, ", ")))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// JiraWiki renders the scenario table with thumbnails of the uploaded screenshots.
func (e *Evidence) JiraWiki() string {
	var b strings.Builder
	b.WriteString("*E2E (local browser):* " + jiraIcons(e.Line()) + "\n")
	if e.Environment != "" {
		b.WriteString("_Environment:_ " + wikiCell(e.Environment) + "\n")
	}
	used := map[string]bool{}
	if len(e.Scenarios) > 0 {
		b.WriteString("||#||Scenario||Expected||Result||Screenshots||\n")
		for _, s := range e.Scenarios {
			var imgs []string
			for _, shot := range s.Screenshots {
				used[shot] = true
				imgs = append(imgs, e.image(shot))
			}
			fmt.Fprintf(&b, "|%s|%s|%s|%s|%s|\n", wikiCell(s.ID), wikiCell(s.Name), wikiCell(s.Expected),
				jiraIcons(resultIcon(s.Result)), strings.Join(imgs, " "))
		}
	}
	var rest []string
	for _, shot := range e.Shots {
		if !used[shot] {
			rest = append(rest, e.image(shot))
		}
	}
	if len(rest) > 0 {
		b.WriteString("Other screenshots: " + strings.Join(rest, " ") + "\n")
	}
	return b.String()
}

func (e *Evidence) image(shot string) string {
	if name, ok := e.Uploaded[shot]; ok {
		return "!" + name + "|thumbnail!"
	}
	return "{{" + wikiCell(shot) + "}} (not uploaded)"
}

func resultIcon(r string) string {
	switch strings.ToUpper(strings.TrimSpace(r)) {
	case "PASS":
		return "✅ PASS"
	case "FAIL":
		return "❌ FAIL"
	default:
		return "⚠️ " + r
	}
}

func jiraIcons(s string) string {
	return strings.NewReplacer("✅", "(/)", "❌", "(x)", "⚠️", "(!)", "`", "").Replace(s)
}

func mdCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(strings.TrimSpace(s))
}

func wikiCell(s string) string {
	s = strings.NewReplacer("|", "¦", "\n", " ", "{", "(", "}", ")", "!", `\!`).Replace(strings.TrimSpace(s))
	if s == "" {
		return " "
	}
	return s
}
