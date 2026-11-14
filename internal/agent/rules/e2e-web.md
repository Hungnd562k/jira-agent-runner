# E2E web verification rules

Required for this job. The fix is not complete until the fixed build is checked in a real browser and screenshots are saved.

## Environment (env vars set by the runner)
- `E2E_BASE_URL` app URL; `E2E_START_CMD` how to start it from the repo root (may be empty = already running).
- `EVIDENCE_DIR` absolute, outside the repo: put scripts, screenshots, logs and `e2e.json` here, never in the repo.
- `E2E_CREDENTIALS_FILE` + `E2E_PROFILE`: JSON `{"profiles": {"<name>": {"role": "...", "web": {"base_url", "username", "password"}}}}`.
- `NODE_PATH` resolves `require('playwright')` when configured.

## Credentials (hard rules)
- Never Read, cat, print, log or echo the credentials file or its values. Load them only inside the Node script:
  `const pr = JSON.parse(require('fs').readFileSync(process.env.E2E_CREDENTIALS_FILE,'utf8')).profiles[process.env.E2E_PROFILE];`
- Never put credentials in command args, URLs, file names, screenshots or `e2e.json`. Do not screenshot the login form after typing.
- Missing file/profile, wrong role or MFA → BLOCKED with the reason. Do not guess or retry logins in a loop.

## Steps
1. Build and start the app from the worktree with your fix (`E2E_START_CMD` in the background, log to `$EVIDENCE_DIR/app.log`). Wait until `E2E_BASE_URL` responds. If a server already holds the port, check it serves THIS worktree; otherwise stop yours and report.
2. Write a Playwright script in `$EVIDENCE_DIR` (Chromium headless, `channel: 'chrome'`, viewport 1600x1000). Log in with the profile, then drive the UI like a user.
3. Scenarios, at least:
   - the original trigger from the issue → fixed behavior;
   - invalid input / error path → message shown and the user can recover on the same screen;
   - one adjacent regression path that shares the changed code.
   Reproduce the bug on the old code first when cheap (e.g. `git stash`, rebuild), and screenshot it as "before".
4. Screenshot each checked state: `NN-short-name.png` (NN = 01, 02…), full page, after the UI settles. Business data may appear; secrets may not.
5. Write `$EVIDENCE_DIR/e2e.json`:
```json
{"environment": "app URL, profile/role, build or commit, data used",
 "scenarios": [{"id": "S1", "name": "...", "steps": "...", "expected": "...",
                "result": "PASS|FAIL|BLOCKED", "screenshots": ["01-short-name.png"]}],
 "blocked": "empty, or why E2E could not be completed"}
```
6. Stop the server and browser you started.

A FAIL means the fix is wrong: fix it and re-run. If E2E cannot run, still write `e2e.json` with `blocked` set; never claim PASS without a current screenshot.
