# jira-agent-runner — Spec

## Goal
Poll Jira for unresolved bugs assigned to me → notify via Telegram with action buttons → (Phase 2) on approval, run a coding agent that produces a draft merge request with test evidence.

## Decisions
| Topic | Decision |
|---|---|
| Language | Go (single static binary, `CGO_ENABLED=0`) |
| Database | SQLite via `modernc.org/sqlite` (pure Go), migrations embedded with `//go:embed` |
| Jira | Server/Data Center, REST API v2 (`/rest/api/2/search`), `Authorization: Bearer <PAT>` |
| Telegram | Bot API, **long polling** (`getUpdates`) — no public URL needed. Lib: `github.com/go-telegram/bot` |
| Deploy | Docker, multi-stage → `gcr.io/distroless/static`, volume `./data:/data`, `mem_limit: 64m` |
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

CLI flags: `--once` (single cycle then exit), `--dry-run` (print to stdout, no Telegram; DB is still updated; Telegram vars not validated), `--env-file` (default `.env`).

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
- Re-reminders reply to the original message.

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
