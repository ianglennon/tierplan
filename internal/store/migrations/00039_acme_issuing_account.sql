-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- Which ACME account placed an order, and which account the stored Let's Encrypt certificate was
-- issued to. A renewal names the certificate it replaces (ARI) only when that certificate was issued
-- to the account placing the renewal: a CA refuses to let one account replace another account's
-- certificate, which is the case after the CA forgot the account and a new one was registered.
--
--   acme_orders.account_url      the account URL the order was created under; an open order placed
--                                under another account than the current one is not resumed.
--   acme_state.cert_account_url  the account URL the stored certificate (tls_certs row "acme") was
--                                issued to. Written only with the certificate, in StoreIssued's
--                                transaction, never by the state row's full update.
--
-- Both are '' when not known (rows from before this migration): an unknown account is never taken
-- to be the current one. Neither column is sealed.

-- +goose Up
ALTER TABLE acme_orders ADD COLUMN account_url TEXT NOT NULL DEFAULT '';
ALTER TABLE acme_state ADD COLUMN cert_account_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE acme_state DROP COLUMN cert_account_url;
ALTER TABLE acme_orders DROP COLUMN account_url;
