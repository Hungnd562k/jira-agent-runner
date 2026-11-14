-- Auto-merge into the shared dev branch and batched handover (dev -> test MR + CI/CD watch).
ALTER TABLE jobs ADD COLUMN mr_iid       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN merged_sha   TEXT    NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN merged_at    INTEGER;          -- set = merged into base_branch, waiting for handover
ALTER TABLE jobs ADD COLUMN handover_id  INTEGER;          -- handover that carries this job
ALTER TABLE jobs ADD COLUMN delivered_at INTEGER;          -- handover CI/CD succeeded

CREATE INDEX idx_jobs_pending_handover ON jobs(merged_at, handover_id);

CREATE TABLE handovers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    status        TEXT NOT NULL,      -- proposed | snoozed | approved | deploying | succeeded | failed
    tg_message_id INTEGER NOT NULL DEFAULT 0,
    message       TEXT NOT NULL DEFAULT '',  -- Telegram HTML of the question (re-rendered on answer)
    remind_at     INTEGER,            -- snoozed: do not propose again before this
    error         TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    deadline_at   INTEGER             -- deploying: give up watching CI/CD after this
);

CREATE INDEX idx_handovers_status ON handovers(status, id);

CREATE TABLE handover_repos (
    handover_id  INTEGER NOT NULL REFERENCES handovers(id),
    repo_name    TEXT    NOT NULL,
    project      TEXT    NOT NULL,
    mr_iid       INTEGER NOT NULL DEFAULT 0,
    mr_url       TEXT    NOT NULL DEFAULT '',
    merge_sha    TEXT    NOT NULL DEFAULT '',
    pipeline_id  INTEGER NOT NULL DEFAULT 0,
    pipeline_url TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL DEFAULT 'pending', -- pending | merged | success | failed
    error        TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (handover_id, repo_name)
);
