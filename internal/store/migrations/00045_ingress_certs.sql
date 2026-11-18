-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- Per-hostname certificates (Phase 20): one Let's Encrypt certificate per hostname over DNS-01, beside
-- TierPlan's own certificate (tls_certs row "acme", acme_state, acme_orders), which is untouched.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set".
--
-- Sealed columns (vault envelopes, all registered in vault.SealedColumns):
--   ingress_certs.key_sealed             sealed with ingress-cert-key/v1/<cert id>
--   ingress_cert_orders.cert_key_sealed  sealed with ingress-order-key/v1/<order id>
--   ingress_acme_account.key_sealed      sealed with ingress-acme-account/v1/<directory_url>
--
-- ingress_certs: one row per certificate of a hostname.
--   id                  UUIDv7. The key's sealing context, so a hostname that moves to another environment
--                       keeps its row and its key and nothing is re-sealed.
--   hostname            lower case (CHECK). Install-wide, as the claims are: the partial unique index
--                       ingress_certs_live admits one LIVE row (retired_at = 0) per hostname. A retired row
--                       may coexist with the live one while it still serves (a retired production certificate).
--   env_id              the environment the hostname belongs to; the row goes with it (CASCADE). Indexed.
--   domain_id           the ingress domain the hostname sits under. No foreign key and not
--                       ingress_hostnames either: a certificate outlives its claim, and a deleted domain
--                       stops renewal instead of deleting the row. Indexed.
--   source              'acme' (ordered by TierPlan) or 'upload' (a certificate the admin supplied).
--   issuer              '' for an upload, otherwise 'staging' or 'production'.
--   retired_at, retired_reason   when the row stopped being the live certificate (0 = live) and why.
--   chain_pem           the certificate chain, PEM. '' until the first issue.
--   key_sealed          the private key, PKCS#8 PEM, sealed. '' until the first issue.
--   serial, not_before, not_after, issued_at   the leaf's serial (lower hex) and validity, and when it
--                       was stored.
--   account_url         the ACME account the certificate was issued to ('' for an upload).
--   phase               'queued', 'ordering', 'idle' (a certificate is held) or 'stopped'.
--   stopped_reason      why a stopped row is stopped.
--   queue_code, queue_used, queue_cap, queue_domain, queue_next_slot_at   why a queued row waits
--                       (a rate-limit budget: how much of which cap is used, and when a slot opens).
--   requested_at, requested_by, unreferenced_since   when and by whom it was asked for, and since when no
--                       claim has referred to the hostname (0 = referred to).
--   consecutive_failures, last_attempt_at, next_attempt_at, retry_after_until, failure_times (JSON
--                       array of instants, '[]' when none), last_error_at, last_error_class,
--                       last_error_detail   the backoff ledger.
--   renew_at, renew_basis, ari_window_start, ari_window_end, ari_checked_at, ari_next_check_at   when the
--                       certificate is next renewed and why, and the renewal-information window.
--   ready               1 when the certificate can be served now. Stored so that a change caused by time
--                       alone can raise ingress_env.certs_version.
--   last_success_at     the last time an order for this row succeeded.
--   created_by, created_at, updated_at.
--
-- ingress_cert_orders: the open (and closed) ACME orders of a certificate row; the acme_orders columns plus
-- account_url and cert_id. A separate table so the own manager's three open-order queries need no scope
-- filter. Deleting a certificate cascades its orders and their sealed keys. Not in backups.
--
-- ingress_cert_issuances: the ledger of issued certificates the rate-limit budget is counted from. The
-- budget belongs to the CA and the registered domain, not to this instance, so it travels in backups.
--
-- ingress_acme_account: the ACME account of the per-hostname manager, one per directory; the columns
-- of acme_account.
--
-- ingress_cert_state: singleton (id 1, seeded). paused_reason is why issuing is paused (a restore),
-- terms_accepted_at and terms_accepted_by the acknowledgement of the CA's terms.
--
-- ingress_env.certs_version: raised by every certificate write for the environment, and never moves
-- config_version, so the reconciler can tell "a certificate changed" from "the routing changed".
--
-- acme_dns_records.credential_ref: which DNS credential created the record; '' for TierPlan's own.

-- +goose Up
CREATE TABLE ingress_certs (
    id                   TEXT PRIMARY KEY,
    hostname             TEXT NOT NULL CHECK (hostname = lower(hostname)),
    env_id               TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    domain_id            TEXT NOT NULL,
    source               TEXT NOT NULL CHECK (source IN ('acme', 'upload')),
    issuer               TEXT NOT NULL DEFAULT '' CHECK (issuer IN ('', 'staging', 'production')),
    retired_at           INTEGER NOT NULL DEFAULT 0,
    retired_reason       TEXT NOT NULL DEFAULT '',
    chain_pem            TEXT NOT NULL DEFAULT '',
    key_sealed           TEXT NOT NULL DEFAULT '',
    serial               TEXT NOT NULL DEFAULT '',
    not_before           INTEGER NOT NULL DEFAULT 0,
    not_after            INTEGER NOT NULL DEFAULT 0,
    issued_at            INTEGER NOT NULL DEFAULT 0,
    account_url          TEXT NOT NULL DEFAULT '',
    phase                TEXT NOT NULL DEFAULT 'queued' CHECK (phase IN ('queued', 'ordering', 'idle', 'stopped')),
    stopped_reason       TEXT NOT NULL DEFAULT '',
    queue_code           TEXT NOT NULL DEFAULT '',
    queue_used           INTEGER NOT NULL DEFAULT 0,
    queue_cap            INTEGER NOT NULL DEFAULT 0,
    queue_domain         TEXT NOT NULL DEFAULT '',
    queue_next_slot_at   INTEGER NOT NULL DEFAULT 0,
    requested_at         INTEGER NOT NULL DEFAULT 0,
    requested_by         TEXT NOT NULL DEFAULT '',
    unreferenced_since   INTEGER NOT NULL DEFAULT 0,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_attempt_at      INTEGER NOT NULL DEFAULT 0,
    next_attempt_at      INTEGER NOT NULL DEFAULT 0,
    retry_after_until    INTEGER NOT NULL DEFAULT 0,
    failure_times        TEXT NOT NULL DEFAULT '[]',
    last_error_at        INTEGER NOT NULL DEFAULT 0,
    last_error_class     TEXT NOT NULL DEFAULT '',
    last_error_detail    TEXT NOT NULL DEFAULT '',
    renew_at             INTEGER NOT NULL DEFAULT 0,
    renew_basis          TEXT NOT NULL DEFAULT '',
    ari_window_start     INTEGER NOT NULL DEFAULT 0,
    ari_window_end       INTEGER NOT NULL DEFAULT 0,
    ari_checked_at       INTEGER NOT NULL DEFAULT 0,
    ari_next_check_at    INTEGER NOT NULL DEFAULT 0,
    ready                INTEGER NOT NULL DEFAULT 0 CHECK (ready IN (0, 1)),
    last_success_at      INTEGER NOT NULL DEFAULT 0,
    created_by           TEXT NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE UNIQUE INDEX ingress_certs_live ON ingress_certs (hostname) WHERE retired_at = 0;
CREATE INDEX ingress_certs_env    ON ingress_certs (env_id);
CREATE INDEX ingress_certs_domain ON ingress_certs (domain_id);

CREATE TABLE ingress_cert_orders (
    id              TEXT PRIMARY KEY,
    cert_id         TEXT NOT NULL REFERENCES ingress_certs (id) ON DELETE CASCADE,
    directory_url   TEXT NOT NULL,
    order_url       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('issue', 'renew')),
    names           TEXT NOT NULL,
    profile         TEXT NOT NULL DEFAULT '',
    replaces        TEXT NOT NULL DEFAULT '',
    account_url     TEXT NOT NULL DEFAULT '',
    cert_key_sealed TEXT NOT NULL,
    stage           TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    resume_count    INTEGER NOT NULL DEFAULT 0,
    expires_at      INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX ingress_cert_orders_cert ON ingress_cert_orders (cert_id, status);

CREATE TABLE ingress_cert_issuances (
    id                TEXT PRIMARY KEY,
    issuer            TEXT NOT NULL CHECK (issuer IN ('staging', 'production')),
    registered_domain TEXT NOT NULL,
    hostname          TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('new', 'renewal')),
    issued_at         INTEGER NOT NULL
);

CREATE INDEX ingress_cert_issuances_domain   ON ingress_cert_issuances (issuer, registered_domain, issued_at);
CREATE INDEX ingress_cert_issuances_hostname ON ingress_cert_issuances (issuer, hostname, issued_at);

CREATE TABLE ingress_acme_account (
    directory_url TEXT PRIMARY KEY,
    account_url   TEXT NOT NULL,
    key_sealed    TEXT NOT NULL,
    contact       TEXT NOT NULL DEFAULT '',
    tos_url       TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);

CREATE TABLE ingress_cert_state (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    paused_reason     TEXT NOT NULL DEFAULT '',
    terms_accepted_at INTEGER NOT NULL DEFAULT 0,
    terms_accepted_by TEXT NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL DEFAULT 0
);

INSERT INTO ingress_cert_state (id) VALUES (1);

ALTER TABLE ingress_env ADD COLUMN certs_version INTEGER NOT NULL DEFAULT 0;

ALTER TABLE acme_dns_records ADD COLUMN credential_ref TEXT NOT NULL DEFAULT '';
CREATE INDEX acme_dns_records_credential ON acme_dns_records (credential_ref);

-- +goose Down
DROP INDEX acme_dns_records_credential;
ALTER TABLE acme_dns_records DROP COLUMN credential_ref;
ALTER TABLE ingress_env DROP COLUMN certs_version;
DROP TABLE ingress_cert_state;
DROP TABLE ingress_acme_account;
DROP INDEX ingress_cert_issuances_hostname;
DROP INDEX ingress_cert_issuances_domain;
DROP TABLE ingress_cert_issuances;
DROP INDEX ingress_cert_orders_cert;
DROP TABLE ingress_cert_orders;
DROP INDEX ingress_certs_domain;
DROP INDEX ingress_certs_env;
DROP INDEX ingress_certs_live;
DROP TABLE ingress_certs;
