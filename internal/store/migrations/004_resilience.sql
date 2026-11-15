-- Resilient jobs: keep partial agent work, resume it, auto-retry transient failures,
-- and reap an agent left running by a crashed worker.
ALTER TABLE jobs ADD COLUMN wip_sha    TEXT    NOT NULL DEFAULT ''; -- failed job: partial work committed on its local branch
ALTER TABLE jobs ADD COLUMN resume_of  INTEGER;                     -- job whose wip_sha this job continues from
ALTER TABLE jobs ADD COLUMN attempt    INTEGER NOT NULL DEFAULT 0;  -- automatic retries so far in this chain
ALTER TABLE jobs ADD COLUMN not_before INTEGER;                     -- queued: not claimed before this (retry backoff)
ALTER TABLE jobs ADD COLUMN agent_pgid INTEGER NOT NULL DEFAULT 0;  -- running: process group of the agent
ALTER TABLE jobs ADD COLUMN boot_id    TEXT    NOT NULL DEFAULT ''; -- host boot the agent_pgid belongs to
