-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- Let's Encrypt over HTTP-01 (Phase 14). Four tables; the issued certificate itself is the
-- tls_certs row "acme", next to the self-signed row, which ACME never touches.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set", so a ledger value reads back
-- equal to the nanosecond.
--
-- Sealed columns (vault envelopes v1.<keyID>.<ciphertext>, both registered in vault.SealedColumns):
--   acme_account.key_sealed     sealed with acme-account/v1/<directory_url>
--   acme_orders.cert_key_sealed sealed with acme-order-key/v1/<order id>
-- The certificate key of an issued certificate moves to tls_certs.key_sealed (tls-key/v1/acme).

-- +goose Up

-- The ACME account, one per directory, so an account made against the staging directory is never
-- used against production. key_sealed is the account's P-256 private key.
CREATE TABLE acme_account (
    directory_url TEXT PRIMARY KEY,
    account_url   TEXT NOT NULL,
    key_sealed    TEXT NOT NULL,
    contact       TEXT NOT NULL DEFAULT '',
    tos_url       TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);

-- The singleton state and ledger (id 1, seeded below, so every reader can UPDATE it without an
-- insert race).
--   mode                   off, or active once a certificate is being kept.
--   paused_reason          why renewal is paused (for example restore); empty when it is not.
--   consecutive_failures   failed runs since the last success.
--   last_attempt_at, next_attempt_at, retry_after_until   the backoff ledger.
--   renew_at, renew_basis  when the certificate is next renewed, and why (fraction, ARI).
--   ari_*                  the renewal-information window and when it was last and is next checked.
CREATE TABLE acme_state (
    id                   INTEGER PRIMARY KEY CHECK (id = 1),
    mode                 TEXT NOT NULL DEFAULT 'off' CHECK (mode IN ('off', 'active')),
    challenge_type       TEXT NOT NULL DEFAULT 'http-01',
    profile              TEXT NOT NULL DEFAULT '',
    paused_reason        TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_attempt_at      INTEGER NOT NULL DEFAULT 0,
    next_attempt_at      INTEGER NOT NULL DEFAULT 0,
    retry_after_until    INTEGER NOT NULL DEFAULT 0,
    renew_at             INTEGER NOT NULL DEFAULT 0,
    ari_window_start     INTEGER NOT NULL DEFAULT 0,
    ari_window_end       INTEGER NOT NULL DEFAULT 0,
    ari_checked_at       INTEGER NOT NULL DEFAULT 0,
    ari_next_check_at    INTEGER NOT NULL DEFAULT 0,
    last_error_at        INTEGER NOT NULL DEFAULT 0,
    last_success_at      INTEGER NOT NULL DEFAULT 0,
    renew_basis          TEXT NOT NULL DEFAULT '',
    last_error_class     TEXT NOT NULL DEFAULT '',
    last_error_detail    TEXT NOT NULL DEFAULT '',
    last_success_kind    TEXT NOT NULL DEFAULT '',
    updated_at           INTEGER NOT NULL DEFAULT 0
);

INSERT INTO acme_state (id) VALUES (1);

-- The open order and its fresh certificate key. The key is sealed and durable before any finalize,
-- so a crash between finalize and storing the certificate cannot lose it. names is a JSON array,
-- in order. Not in backups: a restore must not resurrect an in-flight order.
CREATE TABLE acme_orders (
    id              TEXT PRIMARY KEY,
    directory_url   TEXT NOT NULL,
    order_url       TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('issue', 'renew')),
    names           TEXT NOT NULL,
    profile         TEXT NOT NULL DEFAULT '',
    replaces        TEXT NOT NULL DEFAULT '',
    cert_key_sealed TEXT NOT NULL,
    stage           TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    resume_count    INTEGER NOT NULL DEFAULT 0,
    expires_at      INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

-- The run history, newest 50 kept.
CREATE TABLE acme_runs (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    trigger     TEXT NOT NULL,
    actor       TEXT NOT NULL DEFAULT '',
    names       TEXT NOT NULL DEFAULT '[]',
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0,
    outcome     TEXT NOT NULL DEFAULT 'running',
    error_class TEXT NOT NULL DEFAULT ''
);

CREATE INDEX acme_runs_started ON acme_runs (started_at);

-- +goose Down
DROP INDEX acme_runs_started;
DROP TABLE acme_runs;
DROP TABLE acme_orders;
DROP TABLE acme_state;
DROP TABLE acme_account;
