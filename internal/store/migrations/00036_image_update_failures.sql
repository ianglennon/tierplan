-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- The remote digest an image update failed with, remembered per workload (Phase 12, IMG-07, D-14), so the
-- badge can say "Update failed, rolled back" for that exact digest and stop offering it until a newer one
-- appears. A workload has at most one row: a newer failure replaces it, and a later good update clears it.
-- Nothing here is a credential. Timestamps are UTC unix nanoseconds.
--   env_id           the environment the workload is in; the row goes with it.
--   kind             service or container: what kind of workload failed.
--   workload         the service or container name.
--   digest           the remote digest the update failed with.
--   reason           why it failed.
--   run_id           the update run that failed (image_update_runs.id).
--   rollback_failed  1 when restoring the previous image failed too, 0 when the rollback worked.
--   failed_at        when the failure was recorded.
CREATE TABLE image_update_failures (
    env_id          TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('service', 'container')),
    workload        TEXT NOT NULL,
    digest          TEXT NOT NULL,
    reason          TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    rollback_failed INTEGER NOT NULL DEFAULT 0,
    failed_at       INTEGER NOT NULL,
    PRIMARY KEY (env_id, kind, workload)
);

-- +goose Down
DROP TABLE image_update_failures;
