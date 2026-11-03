/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetImageChecks :many
-- Every check of one repository on one host, whatever its tag and credential. The store keeps the
-- ones it was asked for: a list of composite keys cannot be bound in a query.
SELECT host, repo, tag, cred_id, digest, is_index, platforms, checked_at, success_at, error_code
FROM image_checks
WHERE host = ? AND repo = ?;

-- name: UpsertImageCheckSuccess :exec
-- A check that got an answer: the digest and platforms it found, and no error.
INSERT INTO image_checks (host, repo, tag, cred_id, digest, is_index, platforms, checked_at, success_at, error_code)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '')
ON CONFLICT (host, repo, tag, cred_id) DO UPDATE SET
    digest = excluded.digest,
    is_index = excluded.is_index,
    platforms = excluded.platforms,
    checked_at = excluded.checked_at,
    success_at = excluded.success_at,
    error_code = '';

-- name: UpsertImageCheckFailure :exec
-- A check that failed: when and why. The digest, platforms and time of the last success stay, so the
-- page can say how old the last good answer is.
INSERT INTO image_checks (host, repo, tag, cred_id, checked_at, error_code)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (host, repo, tag, cred_id) DO UPDATE SET
    checked_at = excluded.checked_at,
    error_code = excluded.error_code;
