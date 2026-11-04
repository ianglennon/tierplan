-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- The record of every image update (Phase 12, IMG-06): a row written before anything touches an engine,
-- found again while it runs, and ended exactly once. Nothing here is a credential: registry auth is never
-- stored. Timestamps are UTC unix nanoseconds.
--   id            the run's own ID.
--   env_id        the environment the update ran in; the run goes with it.
--   bulk_id       ties the rows of one bulk update together (D-11); empty for a single update.
--   kind          service, container or stack: what was updated.
--   workload      the service, container or stack name; workload_id its engine ID, stack_id the stack's ID
--                 when the workload belongs to one. Both empty when not known.
--   items         JSON [{service, image, from_digest, to_digest}], one per service or container updated.
--   state         queued, running, done, failed, rolled_back, rollback_failed or refused (D-14).
--   reason        why a run failed, was refused or was rolled back; empty otherwise.
--   actor         the user's ID, or a system name for an automatic update.
--   job_id        the background job that carries the run; empty until it has one.
--   revision_seq  the revision (stack snapshot) sequence number the update wrote; rollback_seq the one a
--                 rollback restored. 0 when there is none.
--   old_container the ID of the container a container update replaced, recorded before it is stopped.
--   started_at    when the run was recorded; finished_at when it ended, 0 while it has not.
CREATE TABLE image_update_runs (
    id            TEXT PRIMARY KEY,
    env_id        TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    bulk_id       TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL CHECK (kind IN ('service', 'container', 'stack')),
    workload      TEXT NOT NULL,
    workload_id   TEXT NOT NULL DEFAULT '',
    stack_id      TEXT NOT NULL DEFAULT '',
    items         TEXT NOT NULL DEFAULT '[]',
    state         TEXT NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed', 'rolled_back', 'rollback_failed', 'refused')),
    reason        TEXT NOT NULL DEFAULT '',
    actor         TEXT NOT NULL,
    job_id        TEXT NOT NULL DEFAULT '',
    revision_seq  INTEGER NOT NULL DEFAULT 0,
    rollback_seq  INTEGER NOT NULL DEFAULT 0,
    old_container TEXT NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX image_update_runs_env_state ON image_update_runs (env_id, state);

-- +goose Down
DROP INDEX image_update_runs_env_state;
DROP TABLE image_update_runs;
