/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: ListIngressIssuancesForDomain :many
-- The instants of the new certificates issued under one registered domain by one issuer after a time, oldest first.
-- A renewal is left out: the CA's per-domain limit does not count it. The ledger is install-wide and survives the
-- deletion and the re-pointing of the certificate rows.
SELECT issued_at FROM ingress_cert_issuances
WHERE issuer = ? AND registered_domain = ? AND kind = 'new' AND issued_at > ?
ORDER BY issued_at;

-- name: ListIngressIssuancesForHost :many
-- The instants of every certificate (new or renewed) issued for one hostname by one issuer after a time, oldest
-- first: the duplicate-certificate limit counts both.
SELECT issued_at FROM ingress_cert_issuances
WHERE issuer = ? AND hostname = ? AND issued_at > ?
ORDER BY issued_at;

-- name: ListIngressOrderStarts :many
-- The instants the account of one directory started an order after a time, oldest first. A closed order is kept for
-- a day, longer than any window asked about.
SELECT created_at FROM ingress_cert_orders WHERE directory_url = ? AND created_at > ? ORDER BY created_at;

-- name: ListOwnACMEIssuances :many
-- The certificates TierPlan's own HTTPS manager obtained after a time: the finished, succeeded issue runs and the
-- names each one covered. Read-only: the own tables are not written by the ingress governor. The table keeps the
-- newest runs only, which is more than a week of them.
SELECT finished_at, names FROM acme_runs
WHERE kind = 'issue' AND outcome = 'succeeded' AND finished_at > ?
ORDER BY finished_at;

-- name: ListGovernedIngressCerts :many
-- The live ACME rows the governor counts against a budget: those with an order in flight (reserved) and those a CA
-- told to wait (a hold on the registered domain). The caller filters by issuer, registered domain and name kind.
SELECT * FROM ingress_certs
WHERE retired_at = 0 AND source = 'acme' AND (phase = 'ordering' OR retry_after_until > sqlc.arg(now));

-- name: PruneIngressIssuances :execrows
-- Deletes the ledger rows issued before a cutoff. The longest window anything asks about is a week.
DELETE FROM ingress_cert_issuances WHERE issued_at < ?;

-- name: ListIngressDomainSuffixesAll :many
-- Every registered domain of every environment with its issuer: the budget view's "shared" flag asks whether
-- another domain sits under the same registered domain.
SELECT id, env_id, suffix, issuer FROM ingress_domains ORDER BY suffix, env_id;
