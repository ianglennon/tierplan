-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- Every run of the janitor, from a person's Clean now and from the schedule (JAN-10, D-16). The row
-- is written before the run removes anything and finalised when it ends, so a run that was cut
-- short is still on record. A backup carries it. Timestamps are UTC unix nanoseconds.
--   trigger      what started the run: the schedule, or a person pressing Clean now.
--   actor        who started it: the user's ID, or the name of the system for a scheduled run.
--   state        waiting and running while it works; completed, partial (an item could not be
--                removed), skipped (it did not run), failed (it could not run) once it has ended.
--   reason       why a run was skipped or failed, '' when it ran: a word the UI maps to its copy.
--   job_id       the job in the jobs hub that does the removing, '' until it exists.
--   started_at   when the run was started; finished_at is 0 while it is not over.
--   removed_*    how many containers, images and networks this run removed.
--   kept         how many selected items the run left alone because they were in use, kept by a
--                label or no longer stale; failed how many could not be removed; skipped_offline
--                how many were not sent because their node was not connected.
--   est_bytes    the bytes the removals freed that were known, -1 when no size was known.
CREATE TABLE janitor_runs (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    trigger            TEXT NOT NULL CHECK (trigger IN ('schedule', 'manual')),
    actor              TEXT NOT NULL,
    state              TEXT NOT NULL CHECK (state IN ('waiting', 'running', 'completed', 'partial', 'skipped', 'failed')),
    reason             TEXT NOT NULL DEFAULT '',
    job_id             TEXT NOT NULL DEFAULT '',
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER NOT NULL DEFAULT 0,
    removed_containers INTEGER NOT NULL DEFAULT 0,
    removed_images     INTEGER NOT NULL DEFAULT 0,
    removed_networks   INTEGER NOT NULL DEFAULT 0,
    kept               INTEGER NOT NULL DEFAULT 0,
    failed             INTEGER NOT NULL DEFAULT 0,
    skipped_offline    INTEGER NOT NULL DEFAULT 0,
    est_bytes          INTEGER NOT NULL DEFAULT -1
);

-- A run is read by its environment and the newest first.
CREATE INDEX janitor_runs_env_started ON janitor_runs (env_id, started_at DESC, id);

-- Each item a run attempted, in the order it attempted them. Removing a run removes its items.
--   seq            1 for the first item the run attempted, counting up; with run_id the key.
--   type           the kind of object.
--   node_id        the node the object is on, node_name its name; '' for an object of the whole swarm.
--   ref            the engine's ID of the object, name its name when the run read it.
--   reason         the code of why the planner selected it (the same words the preview uses) and
--                  reason_detail the rest of the reason as a JSON object (service, slot, rank, ...).
--   outcome        how the attempt ended.
--   code           a stable word for why it did not end as a removal, '' for a clean removal.
--   est_bytes      the bytes removing it frees, -1 when not known.
CREATE TABLE janitor_run_items (
    run_id        TEXT NOT NULL REFERENCES janitor_runs (id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('container', 'image', 'network')),
    node_id       TEXT NOT NULL DEFAULT '',
    node_name     TEXT NOT NULL DEFAULT '',
    ref           TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL,
    reason_detail TEXT NOT NULL DEFAULT '{}',
    outcome       TEXT NOT NULL CHECK (outcome IN ('removed', 'refused_in_use', 'skipped_protected', 'skipped_node_offline', 'error')),
    code          TEXT NOT NULL DEFAULT '',
    est_bytes     INTEGER NOT NULL DEFAULT -1,
    PRIMARY KEY (run_id, seq)
);

-- +goose Down
DROP TABLE janitor_run_items;
DROP INDEX janitor_runs_env_started;
DROP TABLE janitor_runs;
