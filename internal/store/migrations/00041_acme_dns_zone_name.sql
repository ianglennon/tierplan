-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The name of the DNS zone a ledger row's record was created in (Phase 15, review WR-04).
--
--   acme_dns_records.zone_name   the zone's name as the provider reported it when the record was
--                                created, such as 'example.com'. zone_id says where to delete; the
--                                name lets a sweep ask the provider whether that zone still exists
--                                when a delete is refused for a zone ID it does not accept. A record
--                                goes with its zone, so a row whose zone is gone can be dropped.
--
-- '' for rows from before this migration: such a row is never judged gone, and is retried as it
-- always was. Not sealed.

-- +goose Up
ALTER TABLE acme_dns_records ADD COLUMN zone_name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE acme_dns_records DROP COLUMN zone_name;
