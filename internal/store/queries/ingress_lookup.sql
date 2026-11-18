/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetIngressDomainByID :one
-- A domain by its id alone, install-wide. Only the certificate manager reads this way: it holds the
-- domain_id stored on a certificate row, never the environment. The API's own lookups stay environment-scoped.
SELECT * FROM ingress_domains WHERE id = ?;
