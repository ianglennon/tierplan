-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- HTTPS settings that are not Let's Encrypt's (Phase 16): the HSTS toggle and the progress of its
-- ramp. A singleton row, id 1, created here.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set".
--
-- https_state:
--   hsts_enabled     the stored HSTS toggle, 0 (off) by default. TIERPLAN_HSTS overrides it at runtime
--                    and is never written here.
--   hsts_host        the public host the ramp belongs to; HSTS is per host, so another host starts again.
--   hsts_step        where the ramp stands: 0 = not sending, 1..4 = the max-age steps 300 s, 1 week,
--                    1 month, 1 year. Progress is state and not config, so a restart or a redeploy does
--                    not reset it.
--   hsts_step_since  when the current step began; the next step comes after its dwell has passed.
--   hsts_held_until  the instant until which a browser may still hold a policy TierPlan sent: kept at or
--                    after now plus the max-age while sending, never lowered. It is what the page reports
--                    as the time after which no browser can still hold HSTS for this install.
--   updated_at       when the row was last written.

-- +goose Up
CREATE TABLE https_state (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    hsts_enabled    INTEGER NOT NULL DEFAULT 0,
    hsts_host       TEXT    NOT NULL DEFAULT '',
    hsts_step       INTEGER NOT NULL DEFAULT 0,
    hsts_step_since INTEGER NOT NULL DEFAULT 0,
    hsts_held_until INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL DEFAULT 0
);

INSERT INTO https_state (id) VALUES (1);

-- +goose Down
DROP TABLE https_state;
