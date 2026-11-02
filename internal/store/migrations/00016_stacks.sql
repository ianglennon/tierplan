-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- Compose stacks (Phase 7, STK-01, STK-02, STK-05). Timestamps are UTC unix nanoseconds.

-- One row per stack TierPlan manages. target is fixed at creation from the environment kind. source_kind
-- is the source abstraction: inline now, adopted (Phase 8) and git (Phase 9) later; source_ref names the
-- adopted project or the git source. stopped is 1 while the stack is stopped (Phase 7, STK-11).
CREATE TABLE stacks (
    id          TEXT PRIMARY KEY,
    env_id      TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    target      TEXT NOT NULL CHECK (target IN ('swarm', 'standalone')),
    source_kind TEXT NOT NULL DEFAULT 'inline' CHECK (source_kind IN ('inline', 'adopted', 'git')),
    source_ref  TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    stopped     INTEGER NOT NULL DEFAULT 0,
    UNIQUE (env_id, name)
);

-- A stack's definition at a moment: exactly one draft per stack (what the editor saves) and any number of
-- revisions (a copy of the draft taken when a deploy starts). version counts saves of the draft, for the
-- optimistic check. seq numbers revisions per stack; base_seq is the revision a draft started from. outcome
-- marks a revision pending until its deploy ends, then good or failed. rendered and pins are filled when a
-- revision is deployed: the rendered spec and the image digests it ran with. Phase 7 never prunes revisions;
-- Phase 8 owns pruning and rollback to any revision.
CREATE TABLE stack_snapshots (
    id         TEXT PRIMARY KEY,
    stack_id   TEXT NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('draft', 'revision')),
    seq        INTEGER NOT NULL DEFAULT 0,
    base_seq   INTEGER NOT NULL DEFAULT 0,
    version    INTEGER NOT NULL DEFAULT 1,
    outcome    TEXT NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'good', 'failed')),
    rendered   TEXT NOT NULL DEFAULT '',
    pins       TEXT NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX stack_one_draft ON stack_snapshots (stack_id) WHERE kind = 'draft';
CREATE UNIQUE INDEX stack_revision_seq ON stack_snapshots (stack_id, seq) WHERE kind = 'revision';

-- The files of a snapshot (compose.yaml and the extra files beside it). A plain file keeps its text in
-- content; a secret file keeps a vault envelope in content_sealed, sealed under stack-file/v1/<snapshot>/<path>.
CREATE TABLE stack_snapshot_files (
    snapshot_id    TEXT NOT NULL REFERENCES stack_snapshots (id) ON DELETE CASCADE,
    path           TEXT NOT NULL,
    content        TEXT NOT NULL DEFAULT '',
    content_sealed TEXT NOT NULL DEFAULT '',
    secret         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (snapshot_id, path)
);

-- The variables of a snapshot. A secret variable keeps a vault envelope in value_sealed, sealed under
-- stack-var/v1/<snapshot>/<name>.
CREATE TABLE stack_snapshot_vars (
    snapshot_id  TEXT NOT NULL REFERENCES stack_snapshots (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    value        TEXT NOT NULL DEFAULT '',
    value_sealed TEXT NOT NULL DEFAULT '',
    secret       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (snapshot_id, name)
);

-- One row per operation on a stack (a deploy, an update of images, a rollback, a start, a stop, a remove).
-- job_id is the jobs hub job that ran it; failed_service and code say where and why a failed one stopped.
CREATE TABLE stack_deploys (
    id             TEXT PRIMARY KEY,
    stack_id       TEXT NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    env_id         TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('deploy', 'update', 'rollback', 'start', 'stop', 'remove')),
    state          TEXT NOT NULL CHECK (state IN ('running', 'done', 'failed', 'cancelled', 'interrupted')),
    revision_seq   INTEGER NOT NULL DEFAULT 0,
    job_id         TEXT NOT NULL DEFAULT '',
    started_by     TEXT NOT NULL,
    started_at     INTEGER NOT NULL,
    ended_at       INTEGER NOT NULL DEFAULT 0,
    failed_service TEXT NOT NULL DEFAULT '',
    code           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX stack_deploys_by_stack ON stack_deploys (stack_id, started_at);

-- +goose Down
DROP TABLE stack_deploys;
DROP TABLE stack_snapshot_vars;
DROP TABLE stack_snapshot_files;
DROP TABLE stack_snapshots;
DROP TABLE stacks;
