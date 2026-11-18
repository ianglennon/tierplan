/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: CountLiveIngressCertsByDomain :one
-- How many live certificates (not retired) sit under one ingress domain. Shown in the domain list, and counted
-- when the domain is deleted: those certificates stop renewing and expire.
SELECT COUNT(*) FROM ingress_certs WHERE domain_id = ? AND retired_at = 0;

-- name: CountACMEDNSRecordsTokenRefused :one
-- How many of one domain's DNS ledger rows last failed because the provider refused the domain's token
-- (dns_token_invalid or dns_token_scope). The token cannot delete those records, so they do not hold its
-- replacement back (D-27): the replacement is the only thing that can.
SELECT COUNT(*) FROM acme_dns_records
WHERE credential_ref = ? AND last_error_class IN ('dns_token_invalid', 'dns_token_scope');

-- name: RetryACMEDNSRecordsByCredential :execrows
-- Makes one domain's DNS ledger rows due now, with their backoff started again, after the domain's token was
-- replaced: the next sweep tries them with the new token.
UPDATE acme_dns_records SET attempts = 0, next_try_at = 0 WHERE credential_ref = ?;
