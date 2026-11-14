CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    jira_key     TEXT NOT NULL,
    repo_name    TEXT NOT NULL,
    status       TEXT NOT NULL,          -- queued | running | succeeded | failed
    branch       TEXT NOT NULL DEFAULT '',
    mr_url       TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    tests_passed INTEGER,                -- NULL = not run, 0/1
    created_at   INTEGER NOT NULL,
    started_at   INTEGER,
    finished_at  INTEGER
);

CREATE INDEX idx_jobs_status ON jobs(status, id);
CREATE INDEX idx_jobs_key ON jobs(jira_key, id);
