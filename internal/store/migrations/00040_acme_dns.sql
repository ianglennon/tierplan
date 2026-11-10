-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- Let's Encrypt over DNS-01 through Cloudflare (Phase 15): the validation settings, the sealed
-- Cloudflare token, and the ledger of every TXT record TierPlan creates.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set".
--
-- acme_state (written only by the validation save and its own statements, never by the state row's
-- full update, so a stale read cannot undo them; the 00039 precedent):
--   dns_provider         which DNS provider answers a DNS-01 challenge; only 'cloudflare' for now.
--   dns_resolvers        JSON array of normalised host[:port] entries that replace the authoritative
--                        nameservers for the propagation check; '[]' when none are set.
--   validation_untested  1 when the challenge type was changed and no run has proved it yet (the
--                        "Renew now to test it" prompt); 0 once a run has.
--   (challenge_type is already a column; from this migration on only the validation save writes it.)
-- acme_runs:
--   challenge_type       the challenge type the run used; old rows read http-01.
--
-- acme_dns_credentials: one row per provider, so a second provider is a row, not a column.
--   token_sealed         vault envelope sealed with acme-dns-token/v1/<provider>, registered in
--                        vault.SealedColumns. The API never returns it. Travels in backups, so a
--                        restored instance can renew after Resume.
--
-- acme_dns_records: the ledger. One row per TXT record TierPlan created, inserted and checkpointed
-- before the CA is asked to validate, so the provider's record ID is durable first. A delete is by
-- record_id only, never by name or value. NOT in backups: a restored clone must never delete records
-- by the IDs of the install it was copied from.
--   id                   TierPlan's own row ID (UUIDv7).
--   provider, zone_id, record_id   the provider's reference to the record.
--   name, value          the TXT name and content (public once published, so not sealed).
--   comment              the text written to the provider's comment field: tierplan:<install>:<owner>.
--   owner_kind, owner_id the order (or test) the record belongs to.
--   token                the ACME challenge token, so CleanUp finds the rows of one challenge.
--   created_at           when the record was created; leftovers are listed oldest first.
--   attempts, last_try_at, next_try_at, last_error_class   the delete retry state; a row is due when
--                        next_try_at has passed.

-- +goose Up
ALTER TABLE acme_state ADD COLUMN dns_provider TEXT NOT NULL DEFAULT 'cloudflare';
ALTER TABLE acme_state ADD COLUMN dns_resolvers TEXT NOT NULL DEFAULT '[]';
ALTER TABLE acme_state ADD COLUMN validation_untested INTEGER NOT NULL DEFAULT 0;
ALTER TABLE acme_runs ADD COLUMN challenge_type TEXT NOT NULL DEFAULT 'http-01';

CREATE TABLE acme_dns_credentials (
    provider     TEXT PRIMARY KEY,
    token_sealed TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);

CREATE TABLE acme_dns_records (
    id               TEXT PRIMARY KEY,
    provider         TEXT NOT NULL,
    zone_id          TEXT NOT NULL,
    record_id        TEXT NOT NULL,
    name             TEXT NOT NULL,
    value            TEXT NOT NULL,
    comment          TEXT NOT NULL DEFAULT '',
    owner_kind       TEXT NOT NULL CHECK (owner_kind IN ('order', 'test')),
    owner_id         TEXT NOT NULL,
    token            TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    last_try_at      INTEGER NOT NULL DEFAULT 0,
    next_try_at      INTEGER NOT NULL DEFAULT 0,
    last_error_class TEXT NOT NULL DEFAULT ''
);

CREATE INDEX acme_dns_records_next ON acme_dns_records (next_try_at);

-- +goose Down
DROP INDEX acme_dns_records_next;
DROP TABLE acme_dns_records;
DROP TABLE acme_dns_credentials;
ALTER TABLE acme_runs DROP COLUMN challenge_type;
ALTER TABLE acme_state DROP COLUMN validation_untested;
ALTER TABLE acme_state DROP COLUMN dns_resolvers;
ALTER TABLE acme_state DROP COLUMN dns_provider;
