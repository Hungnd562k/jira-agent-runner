# jira-agent-runner
Polls Jira for assigned bugs, requests approval in Telegram, and runs coding agents to produce draft merge requests with test evidence.

## What can this service do?
- Polls un-resolve bug from jira and push notice to a telegram box chat (telegram bot)
- Service must be light weight, take minimal host's resources to get the job done
- Run at local machine (docker)
- Have light weight database to track tasks
- Message notice must has at least link ref to ticket, action buttons: `Fix it`, `Remind me after...` -> time choices buttons
- Untouch notice must be re-remind after <config time>

