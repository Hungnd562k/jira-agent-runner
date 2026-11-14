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
