/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: InsertStackGit :exec
-- The git settings of a stack made from a repository folder (D-06). The sync settings keep their defaults:
-- polling every 5 minutes, auto-deploy off, no webhook (D-14, D-18).
INSERT INTO stack_git (stack_id, repo_id, ref_kind, ref, folder, compose_files, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetStackGit :one
SELECT stack_id, repo_id, ref_kind, ref, folder, compose_files, poll_enabled, poll_interval_s, auto_deploy,
       auto_deploy_paused_reason, auto_deploy_paused_seq, webhook_flavour, webhook_placement, webhook_token_hash,
       webhook_secret_sealed, webhook_secret_last4, webhook_created_by, webhook_last_at, webhook_last_result,
       pending_convert, created_at, updated_at
FROM stack_git WHERE stack_id = ?;

-- name: ListStackGitsByEnv :many
SELECT g.stack_id, g.repo_id, g.ref_kind, g.ref, g.folder, g.compose_files, g.poll_enabled, g.poll_interval_s, g.auto_deploy,
       g.auto_deploy_paused_reason, g.auto_deploy_paused_seq, g.webhook_flavour, g.webhook_placement, g.webhook_token_hash,
       g.webhook_secret_sealed, g.webhook_secret_last4, g.webhook_created_by, g.webhook_last_at, g.webhook_last_result,
       g.pending_convert, g.created_at, g.updated_at
FROM stack_git g JOIN stacks s ON s.id = g.stack_id
WHERE s.env_id = ? ORDER BY s.name;

-- name: InsertStackGitState :exec
-- A new git stack has not been checked: status unknown until the first check.
INSERT INTO stack_git_state (stack_id) VALUES (?);

-- name: GetStackGitState :one
SELECT stack_id, deployed_commit, deployed_tree, head_commit, head_tree, behind, status, reason_code, reason_detail,
       checked_at, error_since, consecutive_failures, pending_commit, next_check_at
FROM stack_git_state WHERE stack_id = ?;

-- name: ListStackGitStatesByEnv :many
SELECT t.stack_id, t.deployed_commit, t.deployed_tree, t.head_commit, t.head_tree, t.behind, t.status, t.reason_code,
       t.reason_detail, t.checked_at, t.error_since, t.consecutive_failures, t.pending_commit, t.next_check_at
FROM stack_git_state t JOIN stacks s ON s.id = t.stack_id
WHERE s.env_id = ? ORDER BY s.name;

-- name: CountStacksUsingRepo :one
SELECT count(*) FROM stack_git WHERE repo_id = ?;

-- name: UpsertStackGitState :exec
-- The one write of a sync check's answer (gitsync's state writer): every column, the row made if a restore left none.
INSERT INTO stack_git_state (stack_id, deployed_commit, deployed_tree, head_commit, head_tree, behind, status, reason_code,
                             reason_detail, checked_at, error_since, consecutive_failures, pending_commit, next_check_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (stack_id) DO UPDATE SET
    deployed_commit = excluded.deployed_commit, deployed_tree = excluded.deployed_tree,
    head_commit = excluded.head_commit, head_tree = excluded.head_tree, behind = excluded.behind,
    status = excluded.status, reason_code = excluded.reason_code, reason_detail = excluded.reason_detail,
    checked_at = excluded.checked_at, error_since = excluded.error_since,
    consecutive_failures = excluded.consecutive_failures, pending_commit = excluded.pending_commit,
    next_check_at = excluded.next_check_at;

-- name: SetStackGitChecked :execrows
-- A check that found nothing new: only the time it looked changes.
UPDATE stack_git_state SET checked_at = ? WHERE stack_id = ?;

-- name: ListPolledStackGits :many
-- The git stacks the background poller looks at (D-14): polling on, and following a branch or a tag (a fixed commit
-- cannot move). The state row is optional: a restore leaves none, and such a stack has not been scheduled yet.
SELECT g.stack_id, s.env_id, g.repo_id, g.ref_kind, g.ref, g.poll_interval_s,
       t.next_check_at, t.checked_at, t.consecutive_failures
FROM stack_git g
JOIN stacks s ON s.id = g.stack_id
LEFT JOIN stack_git_state t ON t.stack_id = g.stack_id
WHERE g.poll_enabled = 1 AND g.ref_kind != 'commit'
ORDER BY g.stack_id;

-- name: SetStackGitNextCheck :exec
-- When the poller looks at the stack next. The row is made if a restore left none.
INSERT INTO stack_git_state (stack_id, next_check_at) VALUES (?, ?)
ON CONFLICT (stack_id) DO UPDATE SET next_check_at = excluded.next_check_at;

-- name: SetStackGitPolling :execrows
-- Polling on or off and how often (D-14). The interval is in seconds, 60 to 86400.
UPDATE stack_git SET poll_enabled = ?, poll_interval_s = ?, updated_at = ? WHERE stack_id = ?;

-- name: ClearStackGitNextCheck :exec
-- A stack whose polling was changed is scheduled afresh by the poller.
UPDATE stack_git_state SET next_check_at = NULL WHERE stack_id = ?;

-- name: SetStackGitSource :execrows
-- The repository, ref, folder and compose files a stack follows, set from the source of the revision that was
-- deployed (the applied settings, D-08).
UPDATE stack_git SET repo_id = ?, ref_kind = ?, ref = ?, folder = ?, compose_files = ?, updated_at = ? WHERE stack_id = ?;
