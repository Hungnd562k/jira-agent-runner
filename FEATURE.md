# FEATURE — Các khả năng của jira-agent-runner

Danh sách các khả năng của service, chia theo PR đã merge vào `main`. Hành vi chi tiết xem [docs/SPEC.md](docs/SPEC.md), luồng làm việc của AI xem [WORKFLOW.md](WORKFLOW.md).

| PR | Tiêu đề | Merge | Branch |
|---|---|---|---|
| [#1](https://github.com/Hungnd562k/jira-agent-runner/pull/1) | MVP notify-only runner (Jira → Telegram, reminders, Docker) | 2026-11-14 | `feat/mvp-notify` |
| [#2](https://github.com/Hungnd562k/jira-agent-runner/pull/2) | Phase 2 — Fix it runs Claude Code and opens a GitLab Draft MR | 2026-11-14 | `feat/phase2-agent` |

---

## PR #1 — MVP: thông báo Jira → Telegram

| # | Nhóm | Khả năng | Chi tiết | Milestone / commit |
|---|---|---|---|---|
| 1.1 | Nền tảng | Một binary Go tĩnh | `CGO_ENABLED=0`, gần như chỉ dùng stdlib; `make build` → `bin/runner` | M0 `bd590ec` |
| 1.2 | Cấu hình | Đọc config từ env / `.env` | `.env` đọc nguyên văn (JQL có `()` không cần quote), env thật được ưu tiên; `--env-file=` để tắt. Kiểm tra `JIRA_BASE_URL` phải là root Jira (từ chối link `/browse/…`, `/secure/…`) | M0, M1 |
| 1.3 | Jira | Lấy bug được assign | REST API v2 `/rest/api/2/search`, Bearer PAT, có phân trang. JQL mặc định: bug chưa resolve gán cho mình, sắp theo priority | M1 `8bf870b` |
| 1.4 | Lưu trữ | SQLite nhẹ | `modernc.org/sqlite` (pure Go), migration nhúng (`user_version`); bảng `tasks` + `events` (audit log chỉ ghi thêm) | M1 |
| 1.5 | Đồng bộ | Mirror kết quả JQL vào DB | Upsert / reopen / close: issue không còn khớp JQL → `closed`, khớp lại → mở lại `new`. **Gọi Jira lỗi thì không bao giờ đóng task** | M1 |
| 1.6 | CLI | `--once`, `--dry-run` | `make dry-run`: chạy 1 vòng, in bảng thay đổi ra stdout, không cần Telegram | M1 |
| 1.7 | Telegram | Client Bot API tự viết | `net/http`, long polling `getUpdates` (không cần URL public), throttle ~1 msg/s, retry 1 lần khi 429, token bị xóa khỏi thông báo lỗi | M2 `b159b6f` |
| 1.8 | Telegram | Gửi notice cho từng bug | `🐞 [KEY] summary`, Priority · Status, link Jira, nút **🛠 Fix it** và **⏰ Remind me after…** | M2 |
| 1.9 | Telegram | Snooze | Remind me after… → hiện các lựa chọn `SNOOZE_CHOICES` (mặc định `30m,2h,24h`) → task chuyển `snoozed` | M2 |
| 1.10 | Telegram | Fix it (MVP) | Chuyển task sang `approved` (ở MVP chưa chạy agent) | M2 |
| 1.11 | Bảo mật | Chỉ nhận lệnh từ chat được cấu hình | Update/callback từ chat khác `TELEGRAM_CHAT_ID` bị bỏ qua | M2 |
| 1.12 | Telegram | Cập nhật notice | Issue đổi trên Jira → sửa notice; issue bị đóng → gỡ nút | M2 |
| 1.13 | Nhắc lại | Nhắc notice chưa ai bấm | Kiểm tra mỗi phút: `notified` không ai bấm quá `REMIND_INTERVAL` (mặc định 4h) → gửi lại dạng reply (footer "🔔 Reminder"), notice cũ bị gỡ nút ("↪ Reminder sent below") | M3 `ee60ae7` |
| 1.14 | Nhắc lại | Hết hạn snooze | `snoozed` quá `remind_at` → nhắc lại đúng giờ. Task `approved` / `closed` không bao giờ được nhắc | M3 |
| 1.15 | Vận hành | Tắt êm | SIGINT/SIGTERM → dừng vòng lặp, thoát code 0 | M3 |
| 1.16 | Triển khai | Docker siêu nhẹ | Multi-stage → distroless static nonroot (~22MB), RAM lúc rảnh ~7.5MiB; compose: `mem_limit 64m`, `cpus 0.25`, rootfs read-only, bỏ capabilities, xoay vòng log, chạy bằng UID của host | M4 `4e00a89` |
| 1.17 | Triển khai | Healthcheck tích hợp | Vòng lặp chính ghi `heartbeat` mỗi phút; `--healthcheck` exit 0 nếu heartbeat mới hơn 3 phút | M4 |
| 1.18 | Vận hành | Lệnh make | `make up` / `down` / `logs` / `run` / `test` / `lint` / `index` | M0–M4 |

**Đã kiểm thử thật:** 15 issue Jira thật, dry-run chạy lại không đổi gì; 15 notice Telegram; Fix it / snooze đúng trong DB và giao diện; 13 notice được nhắc lại sau `REMIND_INTERVAL`; snooze 1m nhắc đúng giờ; container healthy, ~7.4MiB RAM.

---

## PR #2 — Phase 2: Fix it → Claude Code → GitLab Draft MR

| # | Nhóm | Khả năng | Chi tiết | Commit |
|---|---|---|---|---|
| 2.1 | Cấu hình | Map Jira project → repo | `repos.json` (`REPOS_FILE`, git-ignore, mount read-only vào container): mỗi project có nhiều repo ứng viên `{name, gitlab_project, path, base_branch, test_cmd, allowed_tools}` | P2.1 `f816536` |
| 2.2 | Telegram | Fix it theo số repo (đã thay bởi 2.23) | 0 repo → chỉ `approved` ("no repo configured"); 1 repo → `approved` + job `queued` trong cùng 1 transaction; >1 repo → hiện nút chọn repo + Back | P2.1 |
| 2.3 | Lưu trữ | Hàng đợi job | Bảng `jobs` (migration 002): `queued → running → succeeded / failed`, lưu `branch`, `mr_url`, `error`, `tests_passed` | P2.1 |
| 2.4 | Telegram | Tiến độ trên notice | Notice đã approve hiện trạng thái job mới nhất: Queued / Agent running / Done + Draft MR + tests / Failed | P2.1 |
| 2.5 | Worker | Process worker chạy trên host | `runner --worker [--once] [--no-push]`: lấy từng job một; khi khởi động, job còn kẹt ở `running` bị đánh dấu `failed: interrupted`. Dùng chung `data/tasks.db` với notifier, **không gọi `getUpdates`** nên không đụng long polling | P2.2–P2.4 `a6e2770` |
| 2.6 | Jira | Lấy chi tiết issue | Summary, description, type, priority, components, labels, 5 comment gần nhất để làm prompt | P2.2 |
| 2.7 | Git | Mỗi job một worktree | `git fetch` + `git worktree add -b agent/<KEY>-j<id>` từ `origin/<base>`; xong job thì xóa worktree, giữ branch | P2.2 |
| 2.8 | Agent | Chạy Claude Code headless | `claude -p` (prompt qua stdin), `--output-format json`, `acceptEdits`, `allowedTools` giới hạn (Read/Edit/Write/Glob/Grep + `git diff/status/log` + tool riêng của repo), `--max-budget-usd`, `--model` tùy chọn; quá `AGENT_TIMEOUT` thì kill cả process group | P2.2 |
| 2.9 | Agent | Prompt có kỷ luật | Tìm root cause → sửa tối thiểu → thêm test → chạy test; cấm commit/push/sửa CI/secrets; nội dung Jira được coi là dữ liệu chứ không phải lệnh; bắt buộc báo cáo Root cause / Changes / Tests / Risks | P2.2 |
| 2.10 | Kiểm chứng | Worker tự chạy test | Worker tự chạy `test_cmd` (`TEST_TIMEOUT`) → `test.log`. Kết quả test lấy từ worker chứ không tin lời agent; test fail vẫn mở Draft MR nhưng gắn ❌ | P2.3 |
| 2.11 | Git | Commit do worker làm | `fix(<KEY>): <summary>` + link Jira + `Co-Authored-By: Claude`, dùng git identity của host. Agent không sửa gì → `failed: agent made no changes` kèm giải thích | P2.3 |
| 2.12 | GitLab | Mở Draft MR | Push branch → `POST /api/v4/projects/<path>/merge_requests`, title `Draft: [KEY] summary`, target `base_branch`, `remove_source_branch`; mô tả gồm link Jira, kết quả test, báo cáo agent, diff stat, đuôi log test | P2.4 |
| 2.13 | Chế độ an toàn | `--no-push` | `make worker-local`: chỉ commit lên branch local, không push và không mở MR | P2.4 |
| 2.14 | Vận hành | systemd + make | `make worker` / `worker-local`; unit systemd user `deploy/jira-agent-worker.service` | P2.4 |
| 2.15 | Cấu hình | `gitlab_project` nhận clone URL | SSH (`git@host:group/app.git`, `ssh://`) và HTTPS được chuẩn hóa về `group/app` | `56662ee` |
| 2.16 | Git | Worktree đặt cạnh repo clone | Mặc định `<parent>/.agent-<repo>-<KEY>-j<id>` để các tham chiếu `../../other-repo/*.csproj` vẫn build được; `WORKTREE_DIR` để ghi đè. Copy `CLAUDE.md` / `AGENTS.md` / `CLAUDE.local.md` đang bị git-ignore vào để agent theo đúng hướng dẫn dự án (không bao giờ bị commit) | `f061da3` |
| 2.17 | Báo cáo | Report cho mỗi job | `data/jobs/<id>/report.md`, `agent.json`, `test.log`; reply trên Telegram gồm repo, branch (hoặc `git -C <path> show <branch>`) / link MR, tests, chi phí, diff stat, báo cáo của agent | `8e882f7` |
| 2.18 | Jira | Comment lên issue | Khi có Draft MR: comment wiki markup với link MR, branch, tests, báo cáo agent. **Không bao giờ đổi status Jira** | `8e882f7` |
| 2.19 | Telegram | Retry / Other repo (Other repo đã bỏ, xem 2.23) | Job đã xong (Done/Failed) có nút **🔁 Retry** (cùng repo, branch mới `-j<newid>`) và **📦 Other repo**; mỗi task chỉ có tối đa 1 job queued/running. Notifier vẽ lại notice approved khi khởi động để nút mới xuất hiện | `8e882f7` |
| 2.20 | Phục hồi | Báo cả khi fail sau commit | Push/MR lỗi sau khi đã commit → job `failed` nhưng Telegram vẫn báo branch nằm đâu và lỗi ở bước nào | `78b6242` |
| 2.21 | Phục hồi | `--publish-job <id>` | Push nếu cần, mở Draft MR, đánh dấu job succeeded, report + comment Jira; **không chạy lại agent** | `78b6242` |
| 2.22 | Tài liệu | Quick start Phase 2 | README: cách setup `repos.json`, GitLab token, chạy worker | `ae47fe5` |
| 2.23 | Telegram + Worker | Phạm vi sửa từ comment maintainer, job nhiều repo | Fix it/Retry đọc comment `@CLAUDE/CODEX` (`repo_dependencies`, `specs`, `hints`; format trong `scope.json`, sửa không cần code). Thiếu/sai → bot reply hướng dẫn + mẫu copy-paste đã điền sẵn repo. Hợp lệ → 1 nhóm job (mỗi repo 1 job), 1 phiên agent trên mọi worktree, có guard rules chỉ sửa trong phạm vi; test/MR/merge/bàn giao vẫn theo từng repo | branch này |

**Đã kiểm thử thật:** smoke test `claude -p` CLI thật; job HDBCD-2705 trên `cd-boclient-web` (`--no-push`): sửa `BaseFormTextArea.razor` + thêm test mới, 70/70 test pass, test mới fail trên code cũ; GitLab token có quyền Maintainer trên cả 6 repo; job push đầu tiên bị lỗi 403 `insufficient_scope`, đã phục hồi bằng `--publish-job 3` → Draft MR !633 target `dev` + comment Jira.

---

## Đang làm — branch `feat/e2e-evidence` (chưa có PR)

| # | Nhóm | Khả năng | Chi tiết |
|---|---|---|---|
| 3.1 | Agent | Rule file nạp vào session | `internal/agent/rules/fix-bug.md` (luôn nạp) + `e2e-web.md` (repo có `e2e`), embed vào binary, truyền qua `--append-system-prompt` |
| 3.2 | Cấu hình | Khối `e2e` trong `repos.json` | `{base_url, start_cmd, credentials_profile, notes}`: có khối này thì repo bắt buộc phải test trên trình duyệt |
| 3.3 | Bảo mật | Credentials web trong `.secrets/` | `E2E_CREDENTIALS_FILE` (mặc định `.secrets/credentials.json`, git-ignore, 600), cùng format `profiles` với file của NVS; agent chỉ nhận đường dẫn + tên profile, bị deny `Read/Edit/Write` lên file này |
| 3.4 | E2E | Test local + chụp ảnh | Agent chạy app đã sửa, dùng Playwright (Chrome headless, `E2E_NODE_PATH`), chạy kịch bản lỗi gốc / lỗi + phục hồi / hồi quy, lưu `NN-*.png` + `e2e.json` vào `data/jobs/<id>/evidence/` |
| 3.5 | Kiểm chứng | Đánh giá evidence | ✅ chỉ khi mọi kịch bản PASS và có ảnh; thiếu `e2e.json`/ảnh, FAIL hoặc BLOCKED → ⚠️/❌ trên Telegram, `report.md`, MR |
| 3.6 | Jira | Comment kèm ảnh | Upload ảnh lên Jira với tên `<KEY>-j<id>-NN-*.png`, comment có bảng kịch bản + thumbnail, kết thúc bằng dòng `Co-authored-by` |
| 3.7 | Vận hành | Dọn process | Sau mỗi lần chạy agent, worker kill cả process group (app server mà agent bật lên) |
| 3.8 | GitLab | Auto-merge vào dev | Repo bật `auto_merge`: test ✅ + E2E ✅ → MR thường, tự approve, merge vào `base_branch`; không đạt thì Draft MR kèm lý do; merge bị từ chối thì để MR mở và báo |
| 3.9 | Telegram | Hỏi bàn giao | Khi queue rảnh và có ≥ `HANDOVER_MIN_TICKETS` ticket chờ: liệt kê ticket + commit của dev khác trong `dev` chưa có ở `test`; nút **Có** / **Để sau** |
| 3.10 | GitLab | Merge dev → test | Tạo hoặc dùng lại MR `dev → handover_branch`, tự approve, merge (giữ nhánh nguồn) |
| 3.11 | CI/CD | Theo dõi pipeline | Poll pipeline của commit merge; thất bại → job, stage, lý do, 25 dòng log cuối + nút **Theo dõi lại**; có pipeline mới hơn thì theo pipeline đó |
| 3.12 | Jira | Bàn giao ticket | CI/CD ✅ → comment "Đã bàn giao lên test", transition `JIRA_HANDOVER_STATUS`, gán lại reporter; Telegram báo danh sách ticket đã resolve |

---

## Biến môi trường theo phase

| Phase | Biến |
|---|---|
| MVP (#1) | `JIRA_BASE_URL`, `JIRA_PAT`, `JIRA_JQL`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`, `POLL_INTERVAL` (5m), `REMIND_INTERVAL` (4h), `SNOOZE_CHOICES` (30m,2h,24h), `DB_PATH` |
| E2E + bàn giao (branch) | `E2E_CREDENTIALS_FILE`, `E2E_NODE_PATH`, `HANDOVER_MIN_TICKETS`, `PIPELINE_POLL_INTERVAL`, `PIPELINE_START_TIMEOUT`, `PIPELINE_TIMEOUT`, `JIRA_HANDOVER_STATUS`, `JIRA_RESOLUTION` |
| Phase 2 (#2) | `REPOS_FILE`, `SCOPE_FILE`, `GITLAB_BASE_URL`, `GITLAB_TOKEN` (scope `api`), `WORKTREE_DIR`, `CLAUDE_BIN`, `AGENT_TIMEOUT` (30m), `AGENT_MAX_BUDGET_USD` (5), `AGENT_MODEL`, `TEST_TIMEOUT` (15m) |

## Chưa làm / ngoài phạm vi hiện tại

| Hạng mục | Ghi chú |
|---|---|
| Chạy agent trong container sandbox | SPEC ban đầu định chạy trong sandbox; hiện agent chạy trên host, giới hạn bằng `allowedTools`, budget và timeout |
| Chạy song song nhiều job | Worker xử lý tuần tự 1 job một lúc |
