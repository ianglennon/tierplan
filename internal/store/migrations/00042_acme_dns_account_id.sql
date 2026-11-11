-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The provider account a ledger row's zone belongs to (Phase 15, review WR-04).
--
--   acme_dns_records.account_id   the account ID the provider reported for the zone when the record was
--                                 created (Cloudflare's zone.account.id). A delete refused for a zone ID
--                                 is judged to be a zone that went away only inside this account: a token
--                                 for another account sees other zones and says the same thing, and the
--                                 record may well still exist there.
--
-- '' for rows from before this migration: such a row is never judged gone, and is retried as it always
-- was. Not sealed.

-- +goose Up
ALTER TABLE acme_dns_records ADD COLUMN account_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE acme_dns_records DROP COLUMN account_id;
