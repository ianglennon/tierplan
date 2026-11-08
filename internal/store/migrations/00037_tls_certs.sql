-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The server's own TLS certificates (Phase 13.1, D-11). One row per certificate:
-- the default self-signed one is stored under the id "selfsigned", and later
-- certificate sources use other ids, so id is a free string with no CHECK.
-- key_sealed is a vault envelope v1.<keyID>.<ciphertext> sealed with the context
-- tls-key/v1/<id>, so it carries its key ID and cannot be opened from another
-- row. The certificate is public.

-- +goose Up
CREATE TABLE tls_certs (
    id         TEXT PRIMARY KEY,
    cert_pem   TEXT NOT NULL,
    key_sealed TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE tls_certs;
