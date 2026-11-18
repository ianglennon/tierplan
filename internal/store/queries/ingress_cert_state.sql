/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetIngressCertState :one
SELECT * FROM ingress_cert_state WHERE id = 1;

-- name: AcceptIngressCertTerms :exec
-- Records the install-wide acknowledgement of the certificate authority's terms.
UPDATE ingress_cert_state SET terms_accepted_at = ?, terms_accepted_by = ?, updated_at = ? WHERE id = 1;
