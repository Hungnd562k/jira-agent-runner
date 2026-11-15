-- Multi-repo jobs: one "Fix it" queues one job per repo in the maintainer's scope comment,
-- sharing group_id (= id of the group's first job). The worker runs one agent over the whole
-- group; tests/commit/MR/merge/handover stay per job (= per repo). NULL = legacy single job.
ALTER TABLE jobs ADD COLUMN group_id INTEGER;

CREATE INDEX idx_jobs_group ON jobs(group_id);
