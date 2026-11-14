CREATE TABLE tasks (
    jira_key         TEXT PRIMARY KEY,
    summary          TEXT NOT NULL,
    priority         TEXT NOT NULL DEFAULT '',
    jira_status      TEXT NOT NULL DEFAULT '',
    url              TEXT NOT NULL,
    state            TEXT NOT NULL,
    tg_message_id    INTEGER,
    last_notified_at INTEGER,
    remind_at        INTEGER,
    jira_updated     INTEGER,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX idx_tasks_state ON tasks(state);

CREATE TABLE events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    jira_key   TEXT NOT NULL,
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX idx_events_key ON events(jira_key);
