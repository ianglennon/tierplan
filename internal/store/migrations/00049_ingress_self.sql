-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- TierPlan's own route (Phase 28, D-09, Pitfall 11): at most one hostname in the whole install routes to
-- TierPlan itself. The claim is an ordinary ingress_hostnames row with owner_kind 'system' and a NULL
-- stack_id (00044 already allows it) plus one ingress_routes row; nothing about the tables changes.
--
-- ingress_hostnames_one_system is a partial unique index over owner_kind restricted to the 'system' rows,
-- so a second system claim anywhere in the install, whichever environment asks and whatever host it
-- names, fails in the database even if the store's own check were skipped. Stack rows are outside the
-- index and are not limited by it.

-- +goose Up
CREATE UNIQUE INDEX ingress_hostnames_one_system ON ingress_hostnames (owner_kind) WHERE owner_kind = 'system';

-- +goose Down
DROP INDEX ingress_hostnames_one_system;
