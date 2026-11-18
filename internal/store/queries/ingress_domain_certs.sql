/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: CountLiveIngressCertsByDomain :one
-- How many live certificates (not retired) sit under one ingress domain. Shown in the domain list, and counted
-- when the domain is deleted: those certificates stop renewing and expire.
SELECT COUNT(*) FROM ingress_certs WHERE domain_id = ? AND retired_at = 0;
