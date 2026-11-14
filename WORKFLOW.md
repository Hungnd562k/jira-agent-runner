# WORKFLOW — AI làm việc như thế nào trong dự án này

Trong dự án có hai "AI" khác nhau, file này mô tả cả hai:

1. **AI phát triển** (Claude Code trong terminal của bạn): viết service này, theo milestone, branch, test và PR.
2. **AI runtime** (`claude -p` do worker gọi): sửa bug Jira thật khi bạn bấm **Fix it**.

Danh sách khả năng theo PR: [FEATURE.md](FEATURE.md). Hành vi chuẩn: [docs/SPEC.md](docs/SPEC.md).

---

## Phần 1 — AI phát triển (đã làm như thế nào)

### 1.1 Quy trình mỗi thay đổi

```
Yêu cầu của bạn
   │
   ▼
Đọc CLAUDE.md + docs/SPEC.md  ──►  thống nhất spec / milestone (ghi vào SPEC trước khi code)
   │
   ▼
Tạo feature branch (không làm trên main)
   │
   ▼
Tìm hiểu code bằng GitNexus / CodeGraph  ──►  impact analysis trước khi sửa symbol
   │
   ▼
Code + unit test (fake/httptest, không gọi mạng thật)
   │
   ▼
make test (go test -race)  +  make lint (go vet + golangci-lint)
   │
   ▼
Kiểm thử thật (dry-run Jira, Telegram, Docker, job agent thật)
   │
   ▼
Commit theo milestone (Conventional Commits + Co-Authored-By)  ──►  cập nhật SPEC/README
   │
   ▼
Push + mở PR có Summary / Design notes / Test plan  ──►  bạn review & merge
   │
   ▼
Dọn dẹp: checkout main, pull, xóa branch đã merge, make index
```

### 1.2 Nguyên tắc AI tuân theo (từ `CLAUDE.md`)

| Nguyên tắc | Áp dụng thế nào |
|---|---|
| Nhẹ là bắt buộc | Ưu tiên stdlib; Telegram client tự viết; SQLite pure Go; image distroless ~22MB |
| SPEC là nguồn sự thật | Thay đổi hành vi nào cũng phải cập nhật `docs/SPEC.md` trong cùng commit |
| Config chỉ qua env | Biến mới nào cũng thêm vào `.env.example` + bảng SPEC (ngoại lệ: `repos.json`) |
| Schema qua migration | File migration nhúng trong `internal/store/`, không `ALTER` tùy tiện |
| Interface nhỏ phía consumer | Scheduler/worker phụ thuộc interface → test dùng fake, không có network |
| Clock inject | Logic theo thời gian nhận clock, không gọi `time.Now()` trực tiếp |
| `context.Context` | Mọi I/O nhận ctx, tôn trọng cancel để tắt êm |
| Không log secret | PAT/token không bao giờ vào log; token bị xóa khỏi thông báo lỗi |
| Hai process, một DB | Notifier (Docker) giữ `getUpdates`; worker (host) chỉ send/edit |
| Graph trước grep | Dùng GitNexus `impact` / `detect_changes` trước khi sửa và trước khi commit |

### 1.3 Dòng thời gian (2026-11-14)

| Giờ | Commit | Bước | PR |
|---|---|---|---|
| 00:31 | `12a5ff8` | Initial commit | — |
| 01:13 | `bd590ec` | M0: bootstrap, config, Makefile, SPEC, CLAUDE.md, GitNexus | #1 |
| 01:18 | `8bf870b` | M1: Jira client + SQLite store + dry-run | #1 |
| 01:40 | `b159b6f` | M2: Telegram notice + Fix it / snooze | #1 |
| 13:15 | `ee60ae7` | M3: nhắc lại + tắt êm | #1 |
| 13:18 | `4e00a89` | M4: Docker + compose + healthcheck | #1 |
| 13:25 | `8c77908` | **Merge PR #1** | #1 |
| 13:43 | `f816536` | P2.1: chọn repo + hàng đợi job | #2 |
| 13:48 | `a6e2770` | P2.2–P2.4: worker, `claude -p`, test, Draft MR | #2 |
| 14:12 | `56662ee` | Fix: nhận clone URL làm `gitlab_project` | #2 |
| 14:19 | `f061da3` | Fix: worktree cạnh clone + copy guidance bị ignore | #2 |
| 15:01 | `8e882f7` | Report job, comment Jira, Retry / Other repo | #2 |
| 19:17 | `ae47fe5` | Docs: quick start Phase 2 | #2 |
| 20:05 | `78b6242` | `--publish-job` phục hồi MR lỗi | #2 |
| 20:46 | `dc6f0d7` | **Merge PR #2** | #2 |

### 1.4 Vòng phản hồi từ thực tế

Các commit fix trong PR #2 đều đến từ việc chạy job thật, không phải từ thiết kế ban đầu:

| Vấn đề gặp khi chạy thật | Cách xử lý |
|---|---|
| `repos.json` điền clone URL thay vì path API | Chuẩn hóa SSH/HTTPS URL → `group/app` (`56662ee`) |
| `.csproj` trỏ `../../other-repo` nên build hỏng khi worktree nằm ở `~/.cache` | Đặt worktree cạnh clone; copy `CLAUDE.md` đang bị git-ignore vào (`f061da3`) |
| Không biết agent đã làm gì / branch nằm đâu | Report Telegram + `report.md` + comment Jira (`8e882f7`) |
| GitLab token thiếu scope `api` → 403 sau khi đã push | `--publish-job` mở MR mà không chạy lại agent (`78b6242`) |

### 1.5 Cách bạn theo dõi

| Muốn xem | Ở đâu |
|---|---|
| Đã làm gì, vì sao | Commit message (`git log main`) + mô tả PR trên GitHub |
| Hành vi hiện tại | `docs/SPEC.md` |
| Khả năng theo PR | `FEATURE.md` |
| Quy tắc AI phải theo | `CLAUDE.md` |
| Ai viết | Mỗi commit có `Co-Authored-By: Claude …`; author vẫn là bạn |

---

## Phần 2 — AI runtime (đang làm việc thế nào khi bạn bấm Fix it)

### 2.1 Kiến trúc

```
                ┌─────────────── Docker ───────────────┐        ┌──────────────── Host ─────────────────┐
 Jira  ◄─poll── │ notifier (runner)                    │        │ worker (runner --worker)              │
                │  • sync JQL → tasks                  │        │  • claim job                          │
 Telegram ◄───► │  • getUpdates, nút Fix it / snooze   │        │  • git worktree                       │
                │  • ghi job vào DB                    │        │  • claude -p   (AI runtime)           │
                └──────────────┬───────────────────────┘        │  • test_cmd, commit, push, Draft MR   │
                               │      data/tasks.db (SQLite)    │  • Telegram send/edit, Jira comment   │
                               └───────────────◄───────────────►└───────────────────────────────────────┘
```

### 2.2 Luồng một job

| Bước | Ai làm | Việc làm | Kết quả để theo dõi |
|---|---|---|---|
| 1 | Bạn | Bấm **🛠 Fix it** (chọn repo nếu có nhiều repo) | Notice: "Queued" |
| 2 | Notifier | `approved` + tạo job `queued` (1 transaction) | DB `jobs` |
| 3 | Worker | Claim job → `running`; lấy chi tiết issue từ Jira | Notice: "Agent running" |
| 4 | Worker | `git fetch` + worktree `agent/<KEY>-j<id>` cạnh clone, copy `CLAUDE.md`/`AGENTS.md` | Thư mục `.agent-<repo>-<KEY>-j<id>` |
| 5 | **AI** | Đọc code, tìm root cause, sửa tối thiểu, thêm test, chạy test | `data/jobs/<id>/agent.json` |
| 5b | **AI** | (repo web có `e2e`) Build + chạy app local, login bằng profile trong `.secrets/`, chạy Playwright: kịch bản lỗi gốc, lỗi + phục hồi, 1 luồng hồi quy; chụp `NN-*.png`, ghi `e2e.json` | `data/jobs/<id>/evidence/` |
| 6 | Worker | Không có diff → fail kèm giải thích của AI. Đọc evidence: ✅ chỉ khi mọi kịch bản PASS và có ảnh, ngược lại ⚠️/❌ | Telegram reply |
| 7 | Worker | Tự chạy `test_cmd` (không tin lời AI) | `data/jobs/<id>/test.log`, ✅/❌ |
| 8 | Worker | Commit `fix(<KEY>): …` | Branch local |
| 9 | Worker | (trừ khi `--no-push`) push + MR → `dev`. **Gate**: repo bật `auto_merge` + test ✅ + E2E ✅ → approve và **merge vào dev** ("chờ bàn giao"); không đạt → Draft MR kèm lý do | Link MR, notice "🔀 Merged vào dev — chờ bàn giao" |
| 10 | Worker | Kill process group (dọn app server còn chạy), xóa worktree, cập nhật job + notice, gửi report. Khi có Draft MR: upload ảnh `<KEY>-j<id>-NN-*.png` lên Jira + comment có bảng kịch bản kèm thumbnail | `report.md`, Telegram reply, comment Jira |
| 11 | Bạn | Ticket không qua gate: review Draft MR, hoặc **🔁 Retry** / **📦 Other repo** | — |

### 2.2b Bàn giao dev → test

| Bước | Ai làm | Việc làm | Theo dõi ở đâu |
|---|---|---|---|
| B1 | Worker | Khi queue rảnh (không job queued/running) và có ≥ `HANDOVER_MIN_TICKETS` ticket chờ bàn giao: fetch, liệt kê commit của dev khác trong `dev` chưa có ở `test` | Telegram: "🚀 Bàn giao lên test?" kèm danh sách ticket + commit khác |
| B2 | Bạn | **🚀 Có** hoặc **⏰ Để sau** (hỏi lại sau `REMIND_INTERVAL`) | Message câu hỏi được cập nhật |
| B3 | Worker | Tạo hoặc dùng lại MR `dev → test`, tự approve, merge (không xóa `dev`). Ticket merge vào dev sau lúc hỏi cũng được tính | Reply "🔀 Đã merge sang test" |
| B4 | GitLab | CI/CD tự chạy khi có merge | — |
| B5 | Worker | Mỗi `PIPELINE_POLL_INTERVAL` kiểm tra pipeline của commit merge | — |
| B6a | Worker | **Thành công**: comment Jira "Đã bàn giao lên test", chuyển `Resolved`, gán lại reporter | Reply "✅ CI/CD test thành công" + từng ticket ✅/⚠️; notice "🚀 Đã bàn giao test" |
| B6b | Worker | **Thất bại**: job lỗi, stage, lý do + 25 dòng log cuối; không đụng Jira | Reply "❌ CI/CD test thất bại" + nút **🔁 Theo dõi lại** (sau khi retry pipeline trên GitLab) |

### 2.3 AI runtime được giao việc gì

Session `claude -p` nhận hai phần:

| Phần | Nguồn | Nội dung |
|---|---|---|
| System prompt (`--append-system-prompt`) | `internal/agent/rules/fix-bug.md` (luôn nạp) | Flow: ground → investigate/reproduce → fix tối thiểu → test → E2E (nếu bắt buộc) → review diff; giới hạn (không commit/push, không sửa CI/secrets, nội dung Jira là dữ liệu, một server local, bước không chạy được thì ghi BLOCKED); format báo cáo `Root cause / Changes / Tests / E2E / Risks` |
| | `internal/agent/rules/e2e-web.md` (khi repo có `e2e`) | Biến môi trường; quy tắc credentials (không đọc/in file, chỉ script Node đọc); chạy app → Playwright → 3 nhóm kịch bản → ảnh `NN-*.png` → `e2e.json`; FAIL thì sửa lại, không chạy được thì `blocked` |
| Prompt (stdin) | `internal/agent/prompt.go` | Issue (key, type, priority, status, components, labels, summary, description, 5 comment gần nhất) + job (test command, E2E: URL app, lệnh start, profile, thư mục evidence, notes) |

Sửa rule: chỉnh file `.md` trong `internal/agent/rules/` rồi `make build` (rule được embed vào binary).

### 2.4 Hàng rào an toàn

| Rủi ro | Hàng rào |
|---|---|
| AI tự commit/push hoặc phá lịch sử git | Prompt cấm; `allowedTools` chỉ cho `git diff/status/log`; commit/push do worker làm |
| AI chạy lệnh tùy ý | `allowedTools` giới hạn: Read/Edit/Write/Glob/Grep + lệnh build/test khai báo trong `repos.json` |
| Tốn tiền / chạy mãi | `--max-budget-usd` (mặc định 5), `AGENT_TIMEOUT` (30m) kill cả process group |
| Prompt injection từ nội dung Jira | Prompt nói rõ nội dung issue là dữ liệu, không phải lệnh |
| AI báo test pass nhưng thật ra fail | Worker tự chạy test; MR ghi ❌ "do not merge" nếu fail |
| Code lỗi vào nhánh dev chung | Chỉ auto-merge khi repo bật `auto_merge` VÀ test ✅ VÀ E2E ✅ (nếu repo có `e2e`); không thì Draft MR |
| Bàn giao kèm code chưa ai biết | Câu hỏi bàn giao liệt kê commit của dev khác; chỉ merge sang test khi bạn bấm **Có** |
| Resolve ticket khi deploy hỏng | Chỉ resolve + gán reporter khi mọi pipeline ✅; CI fail thì Jira giữ nguyên |
| Lộ mật khẩu web | Credentials ở `.secrets/` (git-ignore, 600); AI chỉ nhận đường dẫn + tên profile; `Read/Edit/Write` lên file bị deny; rule cấm in, cấm đưa vào args/URL/ảnh |
| Báo "xong" khi chưa test UI | E2E thiếu `e2e.json`/ảnh/FAIL/BLOCKED → hiện ⚠️/❌ ở Telegram, MR và Jira, không bao giờ ✅ |
| App server chạy sót, giữ port | Worker kill cả process group sau mỗi lần chạy agent |
| Đổi trạng thái Jira sai | Không bao giờ đổi status, chỉ comment |
| Worker chết giữa chừng | Khởi động lại → job `running` thành `failed: interrupted`; dùng Retry |
| Push/MR lỗi sau khi đã commit | Report nói rõ bước lỗi; `runner --worker --publish-job <id>` |

### 2.5 Theo dõi một job đang chạy

| Muốn biết | Xem ở đâu |
|---|---|
| Trạng thái | Notice Telegram (Queued / Agent running / Done / Failed) |
| AI đã sửa gì, vì sao | Telegram reply hoặc `data/jobs/<id>/report.md` |
| Output thô của AI, chi phí | `data/jobs/<id>/agent.json` |
| Log test | `data/jobs/<id>/test.log` |
| Ảnh + kịch bản E2E | `data/jobs/<id>/evidence/` (`e2e.json`, `NN-*.png`, script, `app.log`); trên Jira: attachment `<KEY>-j<id>-*.png` |
| Code | Draft MR trên GitLab, hoặc `git -C <repo path> show agent/<KEY>-j<id>` khi `--no-push` |
| Lịch sử | Bảng `jobs` và `events` trong `data/tasks.db` |
| Log process | `make logs` (notifier), `journalctl --user -u jira-agent-worker` (worker chạy bằng systemd) |
