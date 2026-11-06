-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- What a registry answered when TierPlan last asked it for the digest of a tag (Phase 12, D-04). A row
-- is the registry's answer, not an environment's: every environment that resolves the same credential
-- for a tag shares one row, and an environment with a credential of its own gets a row of its own
-- (D-19). Docker stays the source of truth: a row is matched to the digest a workload runs live, so
-- nothing here says whether anything is up to date. It holds no credential and no registry text, only
-- a mapped reason code. Timestamps are UTC unix nanoseconds.
--   host        the registry host, with the port when there is one (docker.io for Docker Hub).
--   repo        the repository path below the host (library/nginx).
--   tag         the tag that was asked about.
--   cred_id     the registry credential the check ran with, '' for an anonymous one.
--   digest      the digest the tag pointed at when the last check succeeded, '' before any did.
--   is_index    1 when that digest names a multi-platform index.
--   platforms   a JSON object of platform key (linux/amd64) to that platform's manifest digest.
--   checked_at  when the last check ran, successful or not; success_at when the last successful one did,
--               0 before any did.
--   error_code  why the last check failed, '' when it succeeded: auth, rate_limited, unreachable,
--               not_found, unsupported or other.
CREATE TABLE image_checks (
    host       TEXT NOT NULL,
    repo       TEXT NOT NULL,
    tag        TEXT NOT NULL,
    cred_id    TEXT NOT NULL DEFAULT '',
    digest     TEXT NOT NULL DEFAULT '',
    is_index   INTEGER NOT NULL DEFAULT 0,
    platforms  TEXT NOT NULL DEFAULT '{}',
    checked_at INTEGER NOT NULL DEFAULT 0,
    success_at INTEGER NOT NULL DEFAULT 0,
    error_code TEXT NOT NULL DEFAULT '' CHECK (error_code IN ('', 'auth', 'rate_limited', 'unreachable', 'not_found', 'unsupported', 'other')),
    PRIMARY KEY (host, repo, tag, cred_id)
);

-- +goose Down
DROP TABLE image_checks;
