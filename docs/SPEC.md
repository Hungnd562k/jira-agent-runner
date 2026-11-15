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
| `SCOPE_FILE` | `./scope.json` | format of the maintainer's scope comment (see Scope comment); missing = built-in default (= `scope.example.json`); `/config/scope.json` in Docker |

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
### Repo selection (notifier): maintainer's scope comment
`repos.json` (`REPOS_FILE`, see `repos.example.json`) maps a Jira project key to candidate repos `{name, gitlab_project, path, base_branch, test_cmd, allowed_tools, e2e?, auto_merge, handover_branch}`; `e2e` = `{base_url, start_cmd, credentials_profile, notes}` makes browser verification mandatory for that repo. It is the one config kept outside env because it is a nested list; the file is git-ignored (host paths) and mounted read-only into the container. Missing file = no repos.

A ticket often spans several services, so the repos are not picked with buttons: the maintainer writes a **scope comment** on the Jira ticket, e.g.

```
@CLAUDE/CODEX,
repo_dependencies: cd-host-suite, cd-boclient-web
specs: <for features: the requested change>
hints: <where to look: files, screens, APIs, suspected cause>
```

The format lives in `scope.json` (`SCOPE_FILE`, git-ignored, see `scope.example.json`; `make up` copies the example; missing file = built-in default) and is edited without code changes (restart notifier + worker to reload):
- `marker` — first meaningful line of a scope comment (case-insensitive; trailing `,:;`, `*`/`_` wiki formatting and `---` separator lines are ignored; text after the marker on the same line counts).
- `fields[]` — `{key, role?, required?, placeholder?, description?}`. Exactly one field has `role: "repos"`: its value lists repo names from `repos.json` for the issue's project (separated by `,`/`;`/space/newline). The other fields are guidance passed verbatim to the agent under their key + description. A line `key: value` (or `key = value`) starts a field; following lines that are not another known key continue it (multi-line specs). A value equal to its placeholder or a lone `<…>` counts as empty.
- `authors` — optional Jira usernames/display names allowed to set the scope; empty = anyone. Marked comments from others are ignored.

The latest marked comment (from an allowed author) wins; if it is invalid (required field empty, unknown repo, no repo) it is rejected — an older valid one does not apply.

`Fix it` (and `Retry`) reads the issue's comments (`GetIssue`, before taking the callback lock):
- Project has no repos in `repos.json` → task `approved`, footer "no repo configured" (Jira is not read).
- No valid scope comment → the task is unchanged and the bot replies to the notice with "cần phạm vi sửa từ maintainer": why (missing, or the validation errors), a `<pre>` template (tap-to-copy) prefilled with the marker, every candidate repo and the placeholders, and the meaning of each field. Paste it on the ticket, delete repos not involved, fill specs/hints, press the button again.
- Valid → task `approved` + one **job group** (one `queued` job per scoped repo, same `group_id`) in one transaction.
- Jira unreadable → toast "Không đọc được ticket từ Jira, thử lại", nothing changes.

### Jobs
Table `jobs` (migrations 002–005): `queued → running → succeeded | failed`, `group_id` (jobs queued together by one Fix it / Retry / auto-retry; NULL on legacy rows = own id), with `branch`, `mr_url`, `mr_iid`, `error`, `tests_passed`, `merged_sha`/`merged_at` (in base_branch, waiting for handover), `handover_id`, `delivered_at`, and for resilience `wip_sha` (partial work of a failed job), `resume_of`, `attempt`, `not_before` (retry backoff; not claimed before it), `agent_pgid`/`boot_id` (running agent). Approved notices show the latest group's progress: one line while queued/running (`Agent running on a, b…`), then one line per repo (Done + Draft MR + tests / Merged / Failed / ➖ no changes needed).

### Worker (`runner --worker`, on the host)
Runs where `claude` (logged in), git SSH credentials and repo toolchains exist; shares `./data/tasks.db` with the notifier container. Never calls `getUpdates` (only send/edit), so it does not conflict with the notifier's long polling. One job group at a time (`ClaimGroup` moves the oldest due queued job and the rest of its group to `running`); on start it recovers from a crash (see Resilience).

Pipeline per group (steps 4–9 per job = per repo):
1. `GetIssue` (summary, description, type, priority, components, labels, comments; the prompt gets the last 5) and re-read the scope comment for its guidance fields (specs, hints). Missing now → warn and run on the queued repos without guidance.
2. `git fetch origin <base>` → `git worktree add -b agent/<KEY>-j<jobid> <dir> origin/<base>`, where `<dir>` defaults to `<parent of repo path>/.agent-<repo>-<KEY>-j<jobid>` (same depth as the clone, so relative references to sibling repos like `../../other-repo/x.csproj` resolve) or `<WORKTREE_DIR>/<KEY>-j<jobid>` when set. Git-ignored `CLAUDE.md`/`AGENTS.md`/`CLAUDE.local.md` are copied from the clone so the agent gets project guidance (never committed).
3. One `claude -p` session for the whole group: cwd = first repo's worktree, the other worktrees via `--add-dir`, `allowed_tools` = union of the repos'. The prompt lists the maintainer scope (repos, author, guidance fields) and every repo with its worktree path, test command, E2E and resume note; `fix-bug.md` has **scope guard rules**: edit only inside the listed worktrees, specs decide what / hints where, keep cross-repo contracts consistent, a repo may need no change, a fix needing an out-of-scope repo is not worked around but reported under follow-ups. Agent log `agent.json` is copied into every job dir of the group. Flags: (issue + job on stdin) `--output-format json --permission-mode acceptEdits --allowedTools Read Edit Write Glob Grep Bash(git diff:*) Bash(git status:*) Bash(git log:*) <repo allowed_tools> --append-system-prompt <rules> --max-budget-usd <AGENT_MAX_BUDGET_USD> [--model]`, timeout `AGENT_TIMEOUT`. The process group is killed on timeout and after every run (reaps app servers started for E2E). The agent must not commit/push. Raw output: `data/jobs/<id>/agent.json`. Rules are embedded files in `internal/agent/rules/`: `fix-bug.md` always, `e2e-web.md` when the repo has `e2e` (see E2E below).
4. No changes in any repo → every job `failed: agent made no changes` (agent's explanation sent once as a reply). Some repos changed → the unchanged ones are `succeeded` with note `no changes needed in this repo` (no commit, no report).
5. The worker runs `test_cmd` itself (`sh -c`, timeout `TEST_TIMEOUT`) → `data/jobs/<id>/test.log`; failing tests do not abort.
6. Commit `fix(<KEY>): <summary>` (+ Jira link, `Co-Authored-By: Claude`), host git identity.
7. `--no-push`: stop here (branch kept locally). Otherwise `git push -u origin <branch>` and an MR `[<KEY>] <summary>` → `base_branch` (`remove_source_branch`; description = Jira link, test result, E2E table, agent report, diff stat, test log tail, co-author line). **Merge gate**: repo `auto_merge` AND `test_cmd` ran and passed AND (no `e2e` OR E2E complete ✅). Gate passed → ready MR, approve (errors ignored), wait for mergeability, merge → job `merged_at` (= "chờ bàn giao"). Gate failed → Draft MR (reason in report). A refused merge (conflict, approvals) leaves the MR open and is reported; the job still succeeds.
8. Worktree removed (branch kept); job + notice updated (`Done … Draft MR · tests ✅/❌` or `Failed: …`).
9. Report: `data/jobs/<id>/report.md` (always, once the agent ran) and a Telegram reply on success with repo, branch (+ `git -C <path> show <branch>` when not pushed) or Draft MR link, tests, cost, diff stat and the agent's report. When an MR was opened, the E2E screenshots are uploaded as Jira attachments `<KEY>-j<id>-<file>.png` and a Jira comment (wiki markup) with MR link, branch, tests, the E2E table with thumbnails, the agent report and `{{Co-authored-by: Claude Code <noreply@anthropic.com>}}` is added; the Jira status is never changed.

If push or MR creation fails after the agent committed, the job is `failed` but the Telegram report still says where the branch is and which step failed. Recover without re-running the agent: `runner --worker --publish-job <id>` (push if needed, open the Draft MR, mark the job succeeded, report + Jira comment).

### Resilience (network loss, crashes)
- **Partial work is kept.** When a job fails after its worktree was created and the agent left uncommitted edits, the worker commits them as `wip(<KEY>): partial agent work of job <id>` on the job's local branch (`--no-verify`, never pushed) before removing the worktree, and stores `wip_sha`. A final failure with WIP gets a Telegram reply naming the branch. `--publish-job` refuses WIP-only jobs.
- **Resume.** A retry on the same repo (automatic, or manual 🔁 Retry) whose latest job failed with WIP gets `resume_of`: the new worktree is created from fresh `origin/<base>`, the WIP is applied as uncommitted edits (`cherry-pick --no-commit`; on conflict it falls back to the WIP's own base) and the prompt tells the agent to review and finish them. If nothing applies, it starts fresh. The final commit is one clean `fix(...)` commit. Claude sessions are not resumed (`--resume` is cwd-bound and the worktree path changes); the working tree carries the state.
- **Automatic retry of transient failures.** The transiently failed jobs of a group are requeued together as one new group (each resuming its own WIP). A failed job whose error looks like a network/API hiccup (connection refused/reset, DNS, TLS, `i/o timeout`, `Could not resolve host`, API `5xx`/`overloaded`, git network timeout, `interrupted (worker restarted)`) is requeued on the same repo up to `JOB_AUTO_RETRIES` times with `not_before = now + JOB_RETRY_DELAY·2^(attempt-1)` (capped at ~2h); a Telegram reply says so. Agent timeout, no changes, budget exceeded, missing repo are final. Worker shutdown (SIGTERM) mid-job requeues immediately without using an attempt (`interrupted (worker stopped)`). Manual Retry resets the attempt count; it is refused while an auto-retry is queued.
- **Startup recovery.** Jobs left `running` become `failed: interrupted (worker restarted)`. If the job recorded its agent's process group on this same boot (`/proc/sys/kernel/random/boot_id`), the group is SIGKILLed (an agent orphaned by a worker crash outside systemd). Then every registered worktree on an `agent/<KEY>-j<id>` branch in the configured repos is stale: uncommitted edits are saved as that job's WIP, the worktree is removed and `git worktree prune` runs. Interrupted jobs are then auto-retried as above, per group.
- **Git network bound.** `git fetch` (+ worktree add) and `git push` (also in `--publish-job`) are cancelled after `GIT_NET_TIMEOUT`.
- Not covered: Telegram/Jira notifications that fail while offline are logged and dropped (the DB stays correct; the notice re-renders on the next update).

A finished group with a repo not merged yet shows **🔁 Retry** (`rty:<KEY>`): it re-reads the scope comment (edit the comment on Jira to change repos/specs/hints; missing/invalid → the guide is sent) and queues a new group over the scoped repos minus those already merged in the latest group (new branches `-j<newid>`). Only one queued/running group per task. The old repo picker (`repo:`/`orp:` buttons) is gone; such buttons on old messages answer "Unknown action".

### Handover dev → test (repos with `handover_branch`)
Worker loop: run an approved handover → claim one job → if the queue was empty, maybe propose. CI/CD is polled by a background loop.
1. **Propose** when: no job queued/running, no handover proposed/approved/deploying, none snoozed into the future, and ≥ `HANDOVER_MIN_TICKETS` tickets have merged jobs without handover. Per repo the worker fetches and lists `origin/<handover>..origin/<base>` commits (no merges) that are not the agent's (`fix(<KEY>)` / `[<KEY>]`): "other developers' commits ride along". Telegram question (#id) with **🚀 Có, merge sang test** (`hoy:<id>`) / **⏰ Để sau** (`hol:<id>`); the question HTML is stored to re-render it.
2. **Answer** (notifier): yes → `approved`; later → `snoozed` until now + `REMIND_INTERVAL`, jobs released (next proposal recomputes the batch).
3. **Execute** (worker, before the next job): jobs merged since the question are attached; per repo reuse an open `base → handover` MR or create one (`Bàn giao <handover>: KEYS`, source kept), approve (errors ignored), merge (never delete the source). Repos that fail are detached (their tickets wait for the next proposal); none merged → handover `failed`, all released. Otherwise `deploying` with deadline `PIPELINE_TIMEOUT`.
4. **Watch** every `PIPELINE_POLL_INTERVAL`: find the pipeline on `handover` for the merge SHA (none within `PIPELINE_START_TIMEOUT` → failed), then its status. `failed/canceled/skipped/manual` → if a newer pipeline exists on the branch follow it, else record failed jobs (name, stage, failure_reason) + last 25 log lines of the first. Deadline passed → failed. CI is triggered by GitLab itself; the service never triggers or retries pipelines.
5. **Success** (all repos): per ticket Jira comment "Đã bàn giao lên <handover>" (MR, pipeline, co-author), transition to `JIRA_HANDOVER_STATUS` (matched by target status or transition name; `JIRA_RESOLUTION` when the screen has a resolution field), assign to the reporter; jobs `delivered_at`; Telegram reply lists every ticket with its outcome; notices show "🚀 Đã bàn giao test". **Failure**: Telegram reply with reasons + **🔁 Theo dõi lại CI/CD** (`hor:<id>` → `deploying` again with a new deadline, after a retry in GitLab); Jira untouched.

Tables (migration 003): `handovers` (`status` proposed | snoozed | approved | deploying | succeeded | failed, `tg_message_id`, `message`, `remind_at`, `deadline_at`, `error`), `handover_repos` (`mr_iid/url`, `merge_sha`, `pipeline_id/url`, `status` pending | merged | success | failed, `error`).

### E2E browser verification (repos with `e2e`)
- Before the agent runs, the worker creates `data/jobs/<id>/evidence/` (0700, outside the worktree, never committed) and sets for the agent: `E2E_BASE_URL`, `E2E_START_CMD`, `E2E_PROFILE`, `E2E_CREDENTIALS_FILE`, `EVIDENCE_DIR`, `NODE_PATH` (= `E2E_NODE_PATH`). It adds `--add-dir <evidence>`, allows `Bash(node:*) Bash(npx playwright:*) Bash(curl:*) Bash(sleep:*) Bash(kill:*)` and denies `Read/Edit/Write` on the credentials file (only the agent's Node script reads it; values never enter the transcript).
- Credentials file (`E2E_CREDENTIALS_FILE`, default `./.secrets/credentials.json`, git-ignored, 0600): `{"profiles": {"<name>": {"role", "web": {"base_url", "username", "password"}}}}` — same shape as the NVS workspace `agent-credentials.json`. Missing file → the agent reports E2E as BLOCKED.
- The agent starts the fixed app, runs Playwright (headless Chrome), covers the original trigger, an error + recovery path and an adjacent regression path, saves `NN-name.png` screenshots and `e2e.json` `{environment, scenarios: [{id, name, steps, expected, result: PASS|FAIL|BLOCKED, screenshots}], blocked}`.
- The worker reads the evidence after the run: `✅ n/n PASS · m screenshots` only when every scenario passed and screenshots exist; otherwise `❌`/`⚠️ incomplete|BLOCKED`. Missing evidence never fails the job but is shown on Telegram, `report.md`, the MR and the Jira comment. `--publish-job` reloads the evidence.

Worker env: `HANDOVER_MIN_TICKETS` (`1`), `PIPELINE_POLL_INTERVAL` (`1m`), `PIPELINE_START_TIMEOUT` (`15m`), `PIPELINE_TIMEOUT` (`90m`, also read by the notifier for "watch again"), `JIRA_HANDOVER_STATUS` (`Resolved`), `JIRA_RESOLUTION` (`Fixed`), `E2E_CREDENTIALS_FILE`, `E2E_NODE_PATH`, `GITLAB_BASE_URL`, `GITLAB_TOKEN` (`api` scope; required unless `--no-push`), `WORKTREE_DIR` (empty = next to each clone), `CLAUDE_BIN` (`claude`), `AGENT_TIMEOUT` (`30m`), `AGENT_MAX_BUDGET_USD` (`5`), `AGENT_MODEL`, `TEST_TIMEOUT` (`15m`), `JOB_AUTO_RETRIES` (`3`; `0` disables), `JOB_RETRY_DELAY` (`2m`, doubled per attempt), `GIT_NET_TIMEOUT` (`5m`). Run with `make worker-local` (no push) / `make worker`, or the systemd user unit in `deploy/`.
