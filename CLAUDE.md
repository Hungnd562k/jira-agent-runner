# CLAUDE.md

Service that polls Jira (Server/DC) for assigned bugs, asks for approval in Telegram, and (Phase 2) runs a coding agent to open draft GitLab MRs. Full spec: `docs/SPEC.md` — keep it in sync when behavior changes.

## Stack & principles
- Go (see `go.mod`), single static binary, `CGO_ENABLED=0`. Lightweight is a hard requirement: prefer stdlib, add a dependency only when it clearly pays off.
- SQLite via `modernc.org/sqlite`; schema changes go in embedded migration files under `internal/store/`, never ad-hoc `ALTER` in code.
- Jira: REST API v2 with Bearer PAT, plain `net/http`. Telegram: long polling via the own minimal client in `internal/telegram` (`net/http`, no lib).
- Logging: `log/slog`. Never log tokens/PATs.
- Config only from env (`internal/config`); every new var goes into `.env.example` and the SPEC table.

## Layout
```
cmd/runner/          entrypoint, flags --once / --dry-run
internal/config/     env loading + validation
internal/jira/       Jira client
internal/store/      SQLite store + migrations
internal/telegram/   bot, message rendering, callback handling
internal/scheduler/  poll + remind loops (inject a clock for tests)
docs/SPEC.md         source of truth for behavior
```

## Commands
- `make build` — static binary to `bin/runner`
- `make test` — `go test -race ./...`
- `make lint` — `go vet` + `golangci-lint`
- `make dry-run` — loads `.env`, one cycle, prints issues (no Telegram)
- `make run` — run service locally with `.env`
- `make index` — refresh GitNexus index (`gitnexus analyze .`)

## Conventions
- Accept `context.Context` as first arg for anything doing I/O; honor cancellation for graceful shutdown.
- Depend on small interfaces at consumer side (e.g. scheduler depends on `IssueSource`, `Notifier`, `Store`) so tests use fakes; no network in unit tests (use `httptest.Server`).
- Time-dependent logic takes a clock, never calls `time.Now()` directly inside scheduler/state code.
- Only accept Telegram updates/callbacks from the configured `TELEGRAM_CHAT_ID`.
- Run `make test` and `make lint` before committing.

## GitNexus
Index lives in `.gitnexus/` (git-ignored). Re-run `make index` after each milestone or larger change. Rules for using it are in the generated block below.

## Commits
Work on a feature branch, not `main`. Commits with changes made by Codex must end with exactly one trailer `Co-authored-by: Codex <noreply@openai.com>` (see global instructions); keep the user's author identity.

<!-- gitnexus:start -->
# GitNexus — Code Intelligence

This project is indexed by GitNexus as **jira-agent-runner** (55 symbols, 81 relationships, 3 execution flows).

> Index stale? Run `node .gitnexus/run.cjs analyze --index-only` from the project root — it auto-selects an available runner. No `.gitnexus/run.cjs` yet? Bootstrap with `npx`, `bunx`, or `pnpm dlx` — e.g. `bunx gitnexus@latest analyze` (npm 11 npx crash; #1939).

## Always Do

- **MUST run impact before editing.** Use `impact({target: "symbolName", direction: "upstream"})` or `node .gitnexus/run.cjs impact "symbolName" --direction upstream --repo .`; report callers, processes, and risk. Never substitute grep for graph analysis.
- **MUST analyze graph changes before committing.** Use `detect_changes({scope: "all"})` (MCP) or `node .gitnexus/run.cjs detect-changes --scope all --repo .` (CLI fallback). `partial: true` or `truncated: true` is not a clean check — a zero means unseen, not unaffected; re-run it. For regression review: `detect_changes({scope: "compare", base_ref: "main"})` or `node .gitnexus/run.cjs detect-changes --scope compare --base-ref "main" --repo .`.
- MUST warn on HIGH/CRITICAL `risk` pre-edit; never use `riskSharedAxes` to waive a HIGH/CRITICAL `risk` warning. Compare File/symbol: MCP File omits axes; Graph-RAG expands File.
- **MUST treat `risk: UNKNOWN` as unresolved, not as low.** An empty caller set is not evidence the symbol is unused — it can also mean the callers are not resolvable by the index (plain-object property access, dynamic dispatch, cross-language calls). `impact` pairs `UNKNOWN` with a `riskNote` saying so. Confirm with a text search before treating the symbol as safe to change or delete; do not proceed on the strength of a zero.
- **MUST use `query({search_query: "concept"})` for concepts/flows, `context({name: "symbolName"})` for a named symbol, or `impact` for blast radius, on read-only callers, dependencies, imports, or execution flow.** Graph first; text search only for empty/`UNKNOWN`/literals.
- For security review, `explain({target: "fileOrSymbol"})` lists taint findings (source→sink flows; needs `analyze --pdg`).

## Never Do

- NEVER edit a function, class, or method before MCP/CLI impact analysis.
- NEVER ignore HIGH or CRITICAL risk warnings from impact analysis, and never read `UNKNOWN` as an all-clear — it means the walk could not answer, which is the one verdict that requires confirming by other means.
- NEVER rename symbols with find-and-replace — use `rename` which understands the call graph.
- NEVER commit before MCP/CLI graph change analysis.

## Resources

| Resource | Use for |
| --- | --- |
| `gitnexus://repo/jira-agent-runner/context` | Codebase overview, check index freshness |
| `gitnexus://repo/jira-agent-runner/clusters` | All functional areas |
| `gitnexus://repo/jira-agent-runner/processes` | All execution flows |
| `gitnexus://repo/jira-agent-runner/process/{name}` | Step-by-step execution trace |

## CLI

| Task | Read this skill file |
| --- | --- |
| Understand architecture / "How does X work?" | `.claude/skills/gitnexus-exploring/SKILL.md` |
| Blast radius / "What breaks if I change X?" | `.claude/skills/gitnexus-impact-analysis/SKILL.md` |
| Trace bugs / "Why is X failing?" | `.claude/skills/gitnexus-debugging/SKILL.md` |
| Rename / extract / split / refactor | `.claude/skills/gitnexus-refactoring/SKILL.md` |
| Tools, resources, schema reference | `.claude/skills/gitnexus-guide/SKILL.md` |
| Index, status, clean, wiki CLI commands | `.claude/skills/gitnexus-cli/SKILL.md` |

<!-- gitnexus:end -->
