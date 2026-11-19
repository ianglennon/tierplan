/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetIngressEnv :one
SELECT * FROM ingress_env WHERE env_id = ?;

-- name: BumpIngressConfigVersion :exec
-- Creates the environment's row on its first change, then raises config_version by one. The only statement
-- that moves the version besides the ingress_hostnames_cascade trigger.
INSERT INTO ingress_env (env_id, config_version, updated_at, updated_by) VALUES (?, 1, ?, ?)
ON CONFLICT (env_id) DO UPDATE SET config_version = ingress_env.config_version + 1,
    updated_at = excluded.updated_at, updated_by = excluded.updated_by;

-- name: InsertIngressDomain :exec
INSERT INTO ingress_domains (id, env_id, suffix, issuer, dns_provider, token_sealed, token_updated_at,
                             created_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetIngressDomainInEnv :one
-- Every domain lookup is environment-scoped: a domain id from another environment is "not found", never a hit.
SELECT * FROM ingress_domains WHERE id = ? AND env_id = ?;

-- name: ListIngressDomainsByEnv :many
SELECT * FROM ingress_domains WHERE env_id = ? ORDER BY suffix;

-- name: SetIngressDomainToken :execrows
-- The only statement that writes the sealed token after insert.
UPDATE ingress_domains SET token_sealed = ?, token_updated_at = ?, updated_at = ? WHERE id = ? AND env_id = ?;

-- name: DeleteIngressDomain :execrows
DELETE FROM ingress_domains WHERE id = ? AND env_id = ?;

-- name: ListIngressHolders :many
-- The stacks whose claims sit under a domain, for the refusal to delete it. A system claim has no stack, so
-- the join to stacks is a LEFT JOIN and stack_id and stack_name are NULL for it.
SELECT h.hostname, h.stack_id, s.name AS stack_name, h.env_id, e.name AS env_name
FROM ingress_hostnames h
JOIN environments e ON e.id = h.env_id
LEFT JOIN stacks s ON s.id = h.stack_id
WHERE h.domain_id = ?
ORDER BY e.name, s.name, h.hostname;

-- name: InsertIngressReserved :execrows
-- A name already reserved changes nothing and reports zero rows.
INSERT INTO ingress_reserved (domain_id, hostname, created_by, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT DO NOTHING;

-- name: DeleteIngressReserved :execrows
DELETE FROM ingress_reserved WHERE domain_id = ? AND hostname = ?;

-- name: ListIngressReservedByDomain :many
SELECT * FROM ingress_reserved WHERE domain_id = ? ORDER BY hostname;

-- name: ListIngressReservedInstall :many
-- Every reserved hostname in the install with its domain and that domain's environment (D-11).
SELECT r.hostname, r.domain_id, d.env_id
FROM ingress_reserved r
JOIN ingress_domains d ON d.id = r.domain_id
ORDER BY r.hostname;

-- name: ListIngressClaimsInstall :many
-- Every claim in the install, whichever environment holds it, with the names the policy reports (D-19).
SELECT h.hostname, h.env_id, h.domain_id, h.owner_kind, h.stack_id, h.claimed_by, h.claimed_at,
       e.name AS env_name, s.name AS stack_name
FROM ingress_hostnames h
JOIN environments e ON e.id = h.env_id
LEFT JOIN stacks s ON s.id = h.stack_id
ORDER BY h.hostname;

-- name: GetIngressClaim :one
SELECT h.hostname, h.env_id, h.domain_id, h.owner_kind, h.stack_id, h.claimed_by, h.claimed_at,
       e.name AS env_name, s.name AS stack_name
FROM ingress_hostnames h
JOIN environments e ON e.id = h.env_id
LEFT JOIN stacks s ON s.id = h.stack_id
WHERE h.hostname = ?;

-- name: InsertIngressHostname :exec
INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListIngressHostnamesByStack :many
SELECT * FROM ingress_hostnames WHERE stack_id = ? ORDER BY hostname;

-- name: DeleteIngressHostnamesByStack :execrows
-- Releases a stack's claims; the routes go with them (CASCADE).
DELETE FROM ingress_hostnames WHERE stack_id = ?;

-- name: InsertIngressRoute :exec
INSERT INTO ingress_routes (id, env_id, hostname, name, path_prefix, service, port, scheme, strip,
                            created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListIngressRoutesByStack :many
-- The routes of the hostnames a stack claims, in a fixed order so two reads compare equal.
SELECT r.* FROM ingress_routes r
JOIN ingress_hostnames h ON h.hostname = r.hostname
WHERE h.stack_id = ?
ORDER BY r.hostname, r.path_prefix;

-- name: ListIngressClaimsByEnv :many
-- Every claim of one environment with the stack that holds it, for the renderer (D-12). A system claim has no
-- stack, so the join is a LEFT JOIN and stack_name and stack_target are NULL for it.
SELECT h.hostname, h.env_id, h.domain_id, h.owner_kind, h.stack_id, h.claimed_by, h.claimed_at,
       s.name AS stack_name, s.target AS stack_target
FROM ingress_hostnames h
LEFT JOIN stacks s ON s.id = h.stack_id
WHERE h.env_id = ?
ORDER BY h.hostname;

-- name: ListIngressRoutesByEnv :many
-- Every route of one environment with the stack that owns its hostname. One owner holds a hostname (Phase 18),
-- so the join through ingress_hostnames is exact. A system claim's routes have NULL stack columns. A fixed
-- order, so two reads compare equal.
SELECT r.id, r.env_id, r.hostname, r.name, r.path_prefix, r.service, r.port, r.scheme, r.strip,
       r.created_at, r.updated_at,
       h.owner_kind, h.stack_id, s.name AS stack_name, s.target AS stack_target
FROM ingress_routes r
JOIN ingress_hostnames h ON h.hostname = r.hostname
LEFT JOIN stacks s ON s.id = h.stack_id
WHERE r.env_id = ?
ORDER BY r.hostname, r.path_prefix;

-- name: ListIngressEnvVersions :many
-- The version pair of every environment that has an ingress_env row, in one read, for the reconciler's poll (D-26).
SELECT env_id, config_version, certs_version FROM ingress_env ORDER BY env_id;

-- name: CountIngressStateByEnv :one
-- What a fold of the Local environment would delete through the cascade (D-12, D-17): domains, the reserved
-- names under them (ingress_reserved has no env_id, so through its domain), hostname claims, routes, and
-- whether ingress is turned on. An ingress_env row with enabled = 0 is only a version counter and counts as
-- nothing.
SELECT
    (SELECT COUNT(*) FROM ingress_domains AS d WHERE d.env_id = ?1) AS domains,
    (SELECT COUNT(*) FROM ingress_reserved AS r JOIN ingress_domains AS rd ON rd.id = r.domain_id
        WHERE rd.env_id = ?1) AS reserved,
    (SELECT COUNT(*) FROM ingress_hostnames AS h WHERE h.env_id = ?1) AS hostnames,
    (SELECT COUNT(*) FROM ingress_routes AS rt WHERE rt.env_id = ?1) AS routes,
    (SELECT COUNT(*) FROM ingress_env AS e WHERE e.env_id = ?1 AND e.enabled = 1) AS enabled;
