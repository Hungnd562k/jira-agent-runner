package agent

import (
	"embed"
	"strings"
)

//go:embed rules/*.md
var rulesFS embed.FS

// Rules returns the rule files loaded into the agent's system prompt
// (`--append-system-prompt`): always the bug-fix rules, plus the E2E web rules when required.
func Rules(e2e bool) string {
	names := []string{"rules/fix-bug.md"}
	if e2e {
		names = append(names, "rules/e2e-web.md")
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		b, err := rulesFS.ReadFile(n)
		if err != nil {
			panic(err) // embedded at build time
		}
		parts = append(parts, strings.TrimSpace(string(b)))
	}
	return strings.Join(parts, "\n\n")
}
