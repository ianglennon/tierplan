-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- The address-space settings of one environment (NET-06, D-21, D-07 as amended). small_subnets
-- is the opt-in to giving a stack's networks a small subnet of its own (0 off, 1 on) and
-- small_subnet_mask the size it gets, /25 to /28 (default /26). address_pools holds the pools a
-- person entered for a standalone host, as a JSON list of {"base": "10.20.0.0/16", "size": 24};
-- an empty list means the daemon's or Docker's own pools are used. address_settings_version
-- counts the saves of either setting and address_settings_by is the ID of the user who saved
-- last, so two admins editing at once never overwrite each other unseen. Every existing
-- environment starts with small subnets off at /26 and no entered pools.
ALTER TABLE environments ADD COLUMN small_subnets INTEGER NOT NULL DEFAULT 0
    CHECK (small_subnets IN (0, 1));
ALTER TABLE environments ADD COLUMN small_subnet_mask INTEGER NOT NULL DEFAULT 26
    CHECK (small_subnet_mask BETWEEN 25 AND 28);
ALTER TABLE environments ADD COLUMN address_pools TEXT NOT NULL DEFAULT '[]';
ALTER TABLE environments ADD COLUMN address_settings_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE environments ADD COLUMN address_settings_by TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE environments DROP COLUMN address_settings_by;
ALTER TABLE environments DROP COLUMN address_settings_version;
ALTER TABLE environments DROP COLUMN address_pools;
ALTER TABLE environments DROP COLUMN small_subnet_mask;
ALTER TABLE environments DROP COLUMN small_subnets;
