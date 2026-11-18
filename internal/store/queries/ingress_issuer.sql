/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- The issuer change of an ingress domain (D-25). The ingressstore reads ingress_certs directly here, instead of
-- through ingresscert, because the count the Admin confirms must be taken inside the same write transaction as the
-- change itself: ingresscert imports ingressstore, and a count taken elsewhere could be stale by the commit.

-- name: CountIngressCertsForIssuerChange :one
-- How many live ACME certificates under the domain a change to this issuer would retire: those ordered from the
-- other issuer. Uploads are not counted: an issuer change leaves them alone (D-24).
SELECT COUNT(*) FROM ingress_certs
WHERE domain_id = ? AND source = 'acme' AND retired_at = 0 AND issuer <> ?;

-- name: ListIngressUploadsForDomain :many
-- The hostnames of the live uploaded certificates under the domain, listed apart in the preview because a change
-- of issuer does not touch them.
SELECT hostname FROM ingress_certs
WHERE domain_id = ? AND source = 'upload' AND retired_at = 0
ORDER BY hostname;

-- name: SetIngressDomainIssuer :execrows
-- The only statement that writes a domain's issuer after it is registered.
UPDATE ingress_domains SET issuer = ?, updated_at = ? WHERE id = ? AND env_id = ?;
