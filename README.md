# jira-agent-runner
Polls Jira for assigned bugs, requests approval in Telegram, and runs coding agents to produce draft merge requests with test evidence.

## What can this service do?
- Polls un-resolve bug from jira and push notice to a telegram box chat (telegram bot)
- Service must be light weight, take minimal host's resources to get the job done
- Run at local machine (docker)
- Have light weight database to track tasks
- Message notice must has at least link ref to ticket, action buttons: `Fix it`, `Remind me after...` -> time choices buttons
- Untouch notice must be re-remind after <config time>

## Quick start
1. `cp .env.example .env` and fill in:
   - `JIRA_BASE_URL` — Jira root, e.g. `https://jira.example.com` (not a board/issue link)
   - `JIRA_PAT` — Jira Personal Access Token
   - `TELEGRAM_BOT_TOKEN` — from @BotFather
   - `TELEGRAM_CHAT_ID` — numeric id: message your bot, then open `https://api.telegram.org/bot<TOKEN>/getUpdates` and copy `message.chat.id`
2. Check Jira access without Telegram: `make dry-run`
3. Run in Docker: `make up` (logs: `make logs`, stop: `make down`). State lives in `./data/tasks.db`.

Only one instance may run per bot token (Telegram long polling) — stop `make run` before `make up`.

Behavior and configuration: [docs/SPEC.md](docs/SPEC.md).

## Phase 2: agent fixes (Fix it → Draft MR)
1. `cp repos.example.json repos.json` and list, per Jira project, the candidate repos: local clone `path` (absolute), `gitlab_project`, `base_branch`, `test_cmd` (must work from the repo root), extra `allowed_tools` for build/test commands.
2. Add `GITLAB_BASE_URL` / `GITLAB_TOKEN` (`api` scope) to `.env`.
3. `make up` (notifier, reloads `repos.json`) and on the host `make worker-local` (local branch only) or `make worker` (push + Draft MR + Jira comment). For a permanent setup use `deploy/jira-agent-worker.service`.
4. Web repos: add an `e2e` block to the repo in `repos.json` and put web logins in `.secrets/credentials.json` (`chmod 600`, format: `credentials.example.json`), or point `E2E_CREDENTIALS_FILE` to an existing file with the same `profiles` format. Set `E2E_NODE_PATH` to a `node_modules` that has `playwright`. The agent then verifies the fix in a local browser; screenshots are attached to the Jira comment.
5. Scope comment: `make up` creates `scope.json` from `scope.example.json`. Before **Fix it**, comment on the ticket (the bot sends this template, prefilled, when it is missing):
   ```
   @CLAUDE/CODEX,
   repo_dependencies: cd-host-suite, cd-boclient-web
   specs: <Dành cho feats, yêu cầu sửa đổi>
   hints: <gợi ý giúp agent khoanh vùng nhanh hơn>
   ```
   Change the marker, field names, descriptions or allowed `authors` in `scope.json` — no code change (restart notifier + worker).
6. Shared dev branch: set `"auto_merge": true` and `"handover_branch": "test"` on the repo. A fix whose tests and E2E pass is merged into dev ("chờ bàn giao"). When the queue is idle the bot asks to hand over: **Có** creates, approves and merges dev → test, watches CI/CD and, on success, resolves each ticket and assigns it back to its reporter.
7. On Telegram: **Fix it** → the bot reads the scope comment and queues one agent run over every repo in `repo_dependencies` (no comment → it replies with the template to paste) → the notice shows progress per repo; one report reply per changed repo gives branch/MR, tests and the agent's root-cause summary. Finished jobs offer **Retry** (re-reads the comment, skips repos already merged). Full report: `data/jobs/<id>/report.md`.

Worktrees are created next to each clone (`<parent>/.agent-<repo>-<KEY>-j<id>`) so relative references to sibling repos build; they are removed after each job.

## Updating after a change
Two parts run separately and both must be rebuilt: the **notifier** in Docker (container `jira-agent-runner`) and the **worker** on the host (systemd user unit `jira-agent-worker`, runs `bin/runner`).

```bash
cd ~/private/jira-agent-runner
git switch main && git pull --ff-only   # after the PR is merged
make test                               # optional, recommended

make up                                 # notifier: rebuild image + restart container
make build                              # worker: rebuild bin/runner
systemctl --user restart jira-agent-worker
```

Check that it is running:

```bash
docker compose ps                          # notifier: "Up ... (healthy)"
make logs                                  # notifier logs (Ctrl+C to quit)
systemctl --user status jira-agent-worker  # worker: "active (running)"
journalctl --user -u jira-agent-worker -f  # worker logs
```

The worker's `worker starting` log line lists the effective settings (`auto_retries`, `retry_delay`, `git_net_timeout`, …), so you can confirm the new build is live.

Notes:
- **Restarting the worker mid-job is safe**: the job saves its partial work (WIP) and is requeued without using a retry attempt. When nothing is urgent, wait for the running job to finish.
- **`.env` only changed**: no rebuild needed, just restart: `docker compose up -d` (notifier) and `systemctl --user restart jira-agent-worker` (worker).
- **`repos.json` or `scope.json` changed**: restart both; the notifier and the worker each read them.
- **Never run `make run` / `make worker` next to the services**: one poller per bot token, one worker only.
- **DB migrations apply automatically**: whichever process opens the DB first.
- **Trying a build without push/MR**: `systemctl --user stop jira-agent-worker`, then `make worker-local`; afterwards `systemctl --user start jira-agent-worker`.
