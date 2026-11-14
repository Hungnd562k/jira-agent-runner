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
5. On Telegram: **Fix it** → pick repo → the notice shows progress; a report reply gives branch/MR, tests and the agent's root-cause summary. Finished jobs offer **Retry** / **Other repo**. Full report: `data/jobs/<id>/report.md`.

Worktrees are created next to each clone (`<parent>/.agent-<repo>-<KEY>-j<id>`) so relative references to sibling repos build; they are removed after each job.
