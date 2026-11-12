/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetHTTPSState :one
SELECT * FROM https_state WHERE id = 1;

-- name: SaveHSTSRamp :exec
-- The ramp: the host it belongs to, its step and when the step began. Never writes the toggle or the
-- held-until bound, so a write from the policy cannot undo the admin's choice.
UPDATE https_state SET hsts_host = ?, hsts_step = ?, hsts_step_since = ?, updated_at = ? WHERE id = 1;

-- name: SaveHSTSHeldUntil :exec
-- The bound on what a browser may still hold. Written apart from the ramp, so it survives a reset.
UPDATE https_state SET hsts_held_until = ?, updated_at = ? WHERE id = 1;

-- name: SetHSTSEnabled :exec
-- The admin's HSTS choice. Written apart from the ramp and the held-until bound, so turning the header
-- off or on never forgets what browsers may still hold.
UPDATE https_state SET hsts_enabled = ?, updated_at = ? WHERE id = 1;
