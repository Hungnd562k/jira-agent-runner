# jira-agent-runner — Spec

## Goal
Poll Jira for unresolved bugs assigned to me → notify via Telegram with action buttons → (Phase 2) on approval, run a coding agent that produces a draft merge request with test evidence.

## Decisions
| Topic | Decision |
|---|---|
| Language | Go (single static binary, `CGO_ENABLED=0`) |
| Database | SQLite via `modernc.org/sqlite` (pure Go), migrations embedded with `//go:embed` |
| Jira | Server/Data Center, REST API v2 (`/rest/api/2/search`), `Authorization: Bearer <PAT>` |
| Telegram | Bot API, **long polling** (`getUpdates`) — no public URL needed. Own minimal client on `net/http` (no dependency); throttled ~1 msg/s, one retry on HTTP 429 |
| Deploy | Docker, multi-stage → `gcr.io/distroless/static-debian12:nonroot` (~22MB), compose: `env_file: .env`, volume `./data:/data`, runs as host UID, `mem_limit: 64m`, `cpus: 0.25`, read-only rootfs, built-in healthcheck. Measured idle RAM ≈ 7.5MiB |
| Phase 2 agent | Claude Code CLI headless (`claude -p`) in sandbox container |
| Phase 2 MR | GitLab API, draft MR (`Draft:` title prefix) |

## Configuration (env)
| Var | Default | Notes |
|---|---|---|
| `JIRA_BASE_URL` | — | required |
| `JIRA_PAT` | — | required |
| `JIRA_JQL` | `assignee = currentUser() AND issuetype = Bug AND resolution = Unresolved ORDER BY priority DESC` | |
| `TELEGRAM_BOT_TOKEN` | — | required unless `--dry-run` |
| `TELEGRAM_CHAT_ID` | — | required unless `--dry-run`; updates from other chats are ignored |
| `POLL_INTERVAL` | `5m` | Jira poll period |
| `REMIND_INTERVAL` | `4h` | re-remind untouched notices |
| `SNOOZE_CHOICES` | `30m,2h,24h` | buttons for "Remind me after…" |
| `DB_PATH` | `./data/tasks.db` | `/data/tasks.db` in Docker |

CLI flags: `--once` (single cycle then exit), `--dry-run` (print to stdout, no Telegram; DB is still updated; Telegram vars not validated), `--env-file` (default `.env`; empty disables — used in Docker), `--healthcheck` (exit 0 if `<dir of DB_PATH>/heartbeat` is < 3m old; the main loop rewrites it every minute).

`.env` is read by the binary itself with literal values (same as `docker compose env_file`): no shell quoting needed, real env vars take precedence. `JIRA_BASE_URL` must be the Jira root — page URLs (`/secure/…`, `/browse/…`, query strings) are rejected. `TELEGRAM_CHAT_ID` is the numeric chat id, not the bot username.

Sync rules: a failed Jira request never closes tasks; a closed task that matches the JQL again is reopened as `new`.

## Data model
`tasks`: `jira_key` PK, `summary`, `priority`, `jira_status`, `url`, `state`, `tg_message_id`, `last_notified_at`, `remind_at`, `jira_updated`, `created_at`, `updated_at`.
`events`: append-only audit log (`id`, `jira_key`, `type`, `payload`, `created_at`).

## State machine
```
new ──send──▶ notified ──(untouched > REMIND_INTERVAL)──▶ notified (re-sent)
                 │  ├──"Remind me after… <d>"──▶ snoozed ──(remind_at reached)──▶ notified
                 │  └──"Fix it"──▶ approved   (MVP: stub; Phase 2 worker consumes)
any open state ──(issue no longer matches JQL)──▶ closed (buttons removed)
```
- Callback data (≤64 bytes): `fix:<KEY>`, `snz:<KEY>` (show choices), `snz:<KEY>:<dur>`.
- Due reminders are checked every 1m: `notified` untouched for `REMIND_INTERVAL`, or `snoozed` past `remind_at`. A reminder is sent as a reply to the previous notice (footer "🔔 Reminder"); the previous notice loses its buttons ("↪ Reminder sent below"). `approved`/`closed` are never reminded.
- Long-running mode stops gracefully on SIGINT/SIGTERM.

## Message format
```
🐞 [PROJ-123] <summary>
Priority: High · Status: Open
<JIRA_BASE_URL>/browse/PROJ-123
[🛠 Fix it] [⏰ Remind me after…]
```

## Milestones
- **M0** bootstrap: module, skeleton, config, Makefile, CLAUDE.md, GitNexus index.
- **M1** Jira client + store; `--once --dry-run` prints bugs.
- **M2** Telegram notify + callbacks.
- **M3** scheduler: poll, re-remind, snooze, closed-sync, graceful shutdown; define `Runner` interface.
- **M4** Docker image + compose.
- **Phase 2** worker: `approved` → clone/worktree (project→repo map) → `claude -p` → run tests, collect logs → push branch → GitLab draft MR → comment MR link on Jira + Telegram.

## Phase 2 — agent jobs
### Repo selection (notifier)
`repos.json` (`REPOS_FILE`, see `repos.example.json`) maps a Jira project key to candidate repos `{name, gitlab_project, path, base_branch, test_cmd, allowed_tools}`. It is the one config kept outside env because it is a nested list; the file is git-ignored (host paths) and mounted read-only into the container. Missing file = no repos.

`Fix it` by number of repos for the issue's project:
- 0 → task `approved`, footer "no repo configured".
- 1 → task `approved` + job `queued` (one transaction).
- >1 → repo buttons `repo:<KEY>:<index>` + Back; nothing changes until a repo is picked.

### Jobs
Table `jobs` (migration 002): `queued → running → succeeded | failed`, with `branch`, `mr_url`, `error`, `tests_passed`. Approved notices show the latest job's progress (Queued / Agent running / Done + Draft MR + tests / Failed).

### Worker (`runner --worker`, on the host)
Runs where `claude` (logged in), git SSH credentials and repo toolchains exist; shares `./data/tasks.db` with the notifier container. Never calls `getUpdates` (only send/edit), so it does not conflict with the notifier's long polling. One job at a time; on start, jobs left `running` are marked `failed: interrupted`.

Pipeline per job:
1. `GetIssue` (summary, description, type, priority, components, labels, last 5 comments).
2. `git fetch origin <base>` → `git worktree add -b agent/<KEY>-j<jobid> <WORKTREE_DIR>/<KEY>-j<jobid> origin/<base>`.
3. `claude -p` (prompt on stdin) `--output-format json --permission-mode acceptEdits --allowedTools Read Edit Write Glob Grep Bash(git diff:*) Bash(git status:*) Bash(git log:*) <repo allowed_tools> --max-budget-usd <AGENT_MAX_BUDGET_USD> [--model]`, timeout `AGENT_TIMEOUT` (kills the process group). The agent must not commit/push. Raw output: `data/jobs/<id>/agent.json`.
4. No changes → `failed: agent made no changes` (agent's explanation sent as a reply).
5. The worker runs `test_cmd` itself (`sh -c`, timeout `TEST_TIMEOUT`) → `data/jobs/<id>/test.log`; failing tests do not abort.
6. Commit `fix(<KEY>): <summary>` (+ Jira link, `Co-Authored-By: Claude`), host git identity.
7. `--no-push`: stop here (branch kept locally). Otherwise `git push -u origin <branch>` and `POST /api/v4/projects/<path>/merge_requests` with title `Draft: [<KEY>] <summary>`, target `base_branch`, `remove_source_branch`, description = Jira link, test result (❌ = do not merge), agent report, diff stat, test log tail.
8. Worktree removed (branch kept); job + notice updated (`Done … Draft MR · tests ✅/❌` or `Failed: …`).

Worker env: `GITLAB_BASE_URL`, `GITLAB_TOKEN` (`api` scope; required unless `--no-push`), `WORKTREE_DIR` (`~/.cache/jira-agent-runner/worktrees`), `CLAUDE_BIN` (`claude`), `AGENT_TIMEOUT` (`30m`), `AGENT_MAX_BUDGET_USD` (`5`), `AGENT_MODEL`, `TEST_TIMEOUT` (`15m`). Run with `make worker-local` (no push) / `make worker`, or the systemd user unit in `deploy/`.
