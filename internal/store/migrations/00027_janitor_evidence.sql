-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- The crash evidence of a container the janitor removed whose exit code was not 0 (JAN-10, D-06):
-- why it stopped and the tail of its log, saved before the removal. It lives in its own table so
-- that listing a run's items never reads log text, it goes with its run when the run is pruned, and
-- a backup carries it. Timestamps are UTC unix nanoseconds, 0 when the engine reported none.
--   run_id, seq   the run and the item the evidence belongs to (the key of janitor_run_items; the
--                 evidence is saved before the item row is, so the key is not a foreign key).
--   exit_code     the container's exit code; error the engine's last start error, '' for none.
--   oom_killed    1 when the kernel killed the container for running out of memory.
--   image         the image reference the container was created with, image_id the image it ran and
--                 image_digest the repo@sha256 name of that image on its node, '' when unknown.
--   node          the name of the node it ran on.
--   log_text      the last lines of its log, masked and capped, one line after another; log_lines
--                 how many lines that is.
--   log_state     saved: the lines are in log_text. empty: the container wrote none. unavailable:
--                 its log driver cannot be read back. cap_reached: the run already holds the most
--                 evidence it may, so no log text was kept for this one.
CREATE TABLE janitor_evidence (
    run_id       TEXT NOT NULL REFERENCES janitor_runs (id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    exit_code    INTEGER NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    oom_killed   INTEGER NOT NULL DEFAULT 0,
    started_at   INTEGER NOT NULL DEFAULT 0,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    image        TEXT NOT NULL DEFAULT '',
    image_id     TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    node         TEXT NOT NULL DEFAULT '',
    log_text     TEXT NOT NULL DEFAULT '',
    log_lines    INTEGER NOT NULL DEFAULT 0,
    log_state    TEXT NOT NULL DEFAULT 'saved' CHECK (log_state IN ('saved', 'empty', 'unavailable', 'cap_reached')),
    PRIMARY KEY (run_id, seq)
);

-- +goose Down
DROP TABLE janitor_evidence;
