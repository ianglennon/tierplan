-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Bayplan Authors

-- +goose Up
-- Git-backed stacks (Phase 9, GIT-02, D-06, D-14). A stack with source kind git has one stack_git row: the
-- repository, the ref it tracks, the folder, the ordered compose files and the settings of keeping it in
-- sync. A stack_git_state row is the derived answer of the last check against the repository; it is rebuilt
-- by a check and is not part of a backup.
--
-- repo_id is ON DELETE NO ACTION: a repository that a stack uses cannot be deleted (the store answers
-- repo_in_use first, the key is the backstop). Deleting a stack or its environment removes the rows.
-- compose_files is a JSON array of paths relative to the folder, in merge order. webhook_token_hash is the
-- SHA-256 of the routing token in the webhook address; webhook_secret_sealed is a vault envelope sealed
-- under git-webhook/v1/<stack_id>. Timestamps are UTC unix nanoseconds.
CREATE TABLE stack_git (
    stack_id                  TEXT PRIMARY KEY REFERENCES stacks (id) ON DELETE CASCADE,
    repo_id                   TEXT NOT NULL REFERENCES git_repos (id) ON DELETE NO ACTION,
    ref_kind                  TEXT NOT NULL CHECK (ref_kind IN ('branch', 'tag', 'commit')),
    ref                       TEXT NOT NULL,
    folder                    TEXT NOT NULL DEFAULT '',
    compose_files             TEXT NOT NULL,
    poll_enabled              INTEGER NOT NULL DEFAULT 1,
    poll_interval_s           INTEGER NOT NULL DEFAULT 300 CHECK (poll_interval_s BETWEEN 60 AND 86400),
    auto_deploy               INTEGER NOT NULL DEFAULT 0,
    auto_deploy_paused_reason TEXT NOT NULL DEFAULT '' CHECK (auto_deploy_paused_reason IN ('', 'rollback', 'owner')),
    auto_deploy_paused_seq    INTEGER NOT NULL DEFAULT 0,
    webhook_flavour           TEXT NOT NULL DEFAULT '' CHECK (webhook_flavour IN ('', 'github', 'gitlab', 'gitea', 'generic')),
    webhook_placement         TEXT NOT NULL DEFAULT '' CHECK (webhook_placement IN ('', 'path', 'header')),
    webhook_token_hash        TEXT NOT NULL DEFAULT '',
    webhook_secret_sealed     TEXT NOT NULL DEFAULT '',
    webhook_secret_last4      TEXT NOT NULL DEFAULT '',
    webhook_created_by        TEXT NOT NULL DEFAULT '',
    webhook_last_at           INTEGER,
    webhook_last_result       TEXT NOT NULL DEFAULT '',
    pending_convert           INTEGER NOT NULL DEFAULT 0,
    created_at                INTEGER NOT NULL,
    updated_at                INTEGER NOT NULL
);

CREATE UNIQUE INDEX stack_git_webhook_token ON stack_git (webhook_token_hash) WHERE webhook_token_hash != '';
CREATE INDEX stack_git_repo ON stack_git (repo_id);

CREATE TABLE stack_git_state (
    stack_id             TEXT PRIMARY KEY REFERENCES stacks (id) ON DELETE CASCADE,
    deployed_commit      TEXT NOT NULL DEFAULT '',
    deployed_tree        TEXT NOT NULL DEFAULT '',
    head_commit          TEXT NOT NULL DEFAULT '',
    head_tree            TEXT NOT NULL DEFAULT '',
    behind               INTEGER NOT NULL DEFAULT 0,
    status               TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'in_sync', 'behind', 'error')),
    reason_code          TEXT NOT NULL DEFAULT '',
    reason_detail        TEXT NOT NULL DEFAULT '',
    checked_at           INTEGER,
    error_since          INTEGER,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    pending_commit       TEXT NOT NULL DEFAULT '',
    next_check_at        INTEGER
);

-- +goose Down
DROP TABLE stack_git_state;
DROP INDEX stack_git_repo;
DROP INDEX stack_git_webhook_token;
DROP TABLE stack_git;
