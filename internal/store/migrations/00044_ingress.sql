-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The ingress model (Phase 18): per-environment domains, reserved names, and the install-wide
-- claims and routes a stack's labels turn into.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set".
--
-- ingress_env: one row per environment that has ever had an ingress change.
--   env_id          the environment; the row goes with it.
--   enabled         the ingress switch, 0 (off) by default. Nothing in Phase 18 writes it.
--   config_version  strictly increases on every committed change to the environment's domains, reserved
--                   names, claims or routes, so a reconciler can tell "something changed" from "nothing did".
--                   It rises by one per cascaded claim row, so it is "strictly increases", not "by one".
--   updated_at, updated_by  when and by whom the row was last written.
--
-- ingress_domains: a DNS suffix an environment may serve names under.
--   id               UUIDv7.
--   env_id           the environment. UNIQUE (env_id, suffix): the same suffix may exist in several
--                    environments, each with its own token row; hostnames, not domains, are install-wide.
--   suffix           the registered suffix, lower case, with no leading or trailing dot.
--   issuer           'staging' or 'production'; production by default.
--   dns_provider     the provider that holds the zone; 'cloudflare' only for now.
--   token_sealed     the DNS API token, sealed under vault.ContextIngressDNSToken plus id. '' means none.
--                    Never returned by any endpoint.
--   token_updated_at when the token was last set or replaced (0 = never).
--   created_by, created_at, updated_at  who registered it and when; updated_at moves on any change.
--
-- ingress_reserved: a hostname an admin holds back under a domain so no stack can claim it.
--   domain_id        the domain; reserved names go with it (CASCADE).
--   hostname         exact, lower case, strictly below the domain's suffix. No wildcards.
--
-- ingress_hostnames: who owns a hostname. hostname alone is the primary key, so the database itself
-- refuses a second owner anywhere in the install, whichever environment asks (LBL-03).
--   hostname         lower case (CHECK), the primary key.
--   env_id           the environment that owns it. Not part of the key: it selects per environment.
--   domain_id        the domain it sits under. ON DELETE RESTRICT: a domain in use cannot be deleted.
--   owner_kind       'stack' for a stack's claim, 'system' for one TierPlan itself holds.
--   stack_id         the claiming stack; its claims go with it (CASCADE). NULL only for 'system'.
--   claimed_by, claimed_at  who first claimed it and when.
--
-- ingress_routes: what a claimed hostname serves.
--   hostname         the claim; routes go with it (CASCADE). UNIQUE (hostname, path_prefix).
--   env_id           the claim's environment, kept here for the reconciler; the store writes both from
--                    one variable.
--   name             the route's label name ('' for the default route).
--   path_prefix      the path the route matches, '/' by default.
--   service          the compose service that answers.
--   port             the container port, 1 to 65535.
--   scheme           'http' (default) or 'https', how the proxy reaches the service.
--   strip            1 when the proxy removes path_prefix before forwarding, 0 otherwise.
--
-- Table order matters. SQLite runs foreign-key actions for a parent in the reverse of the order its
-- child tables were created, so ingress_domains is created before ingress_hostnames: removing an
-- environment that holds a claim then removes the claims before the domain row and the RESTRICT does
-- not fire. Do not reorder.
--
-- The trigger ingress_hostnames_cascade covers one path no call site sees: a claim deleted because its
-- stack row was deleted. It bumps the environment's config_version then, and only then: an explicit
-- release (the stack still exists) and an environment removal (the environment is gone) are skipped.

-- +goose Up
CREATE TABLE ingress_env (
    env_id         TEXT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    enabled        INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    config_version INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL DEFAULT 0,
    updated_by     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE ingress_domains (
    id               TEXT PRIMARY KEY,
    env_id           TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    suffix           TEXT NOT NULL,
    issuer           TEXT NOT NULL DEFAULT 'production' CHECK (issuer IN ('staging', 'production')),
    dns_provider     TEXT NOT NULL DEFAULT 'cloudflare',
    token_sealed     TEXT NOT NULL DEFAULT '',
    token_updated_at INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    UNIQUE (env_id, suffix)
);

CREATE TABLE ingress_reserved (
    domain_id  TEXT NOT NULL REFERENCES ingress_domains (id) ON DELETE CASCADE,
    hostname   TEXT NOT NULL CHECK (hostname = lower(hostname)),
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (domain_id, hostname)
);

CREATE TABLE ingress_hostnames (
    hostname   TEXT PRIMARY KEY CHECK (hostname = lower(hostname)),
    env_id     TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    domain_id  TEXT NOT NULL REFERENCES ingress_domains (id) ON DELETE RESTRICT,
    owner_kind TEXT NOT NULL CHECK (owner_kind IN ('stack', 'system')),
    stack_id   TEXT REFERENCES stacks (id) ON DELETE CASCADE,
    claimed_by TEXT NOT NULL,
    claimed_at INTEGER NOT NULL,
    CHECK ((owner_kind = 'stack') = (stack_id IS NOT NULL))
);

CREATE INDEX ingress_hostnames_env    ON ingress_hostnames (env_id);
CREATE INDEX ingress_hostnames_stack  ON ingress_hostnames (stack_id);
CREATE INDEX ingress_hostnames_domain ON ingress_hostnames (domain_id);

CREATE TABLE ingress_routes (
    id          TEXT PRIMARY KEY,
    env_id      TEXT NOT NULL,
    hostname    TEXT NOT NULL REFERENCES ingress_hostnames (hostname) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    path_prefix TEXT NOT NULL DEFAULT '/',
    service     TEXT NOT NULL,
    port        INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    scheme      TEXT NOT NULL DEFAULT 'http' CHECK (scheme IN ('http', 'https')),
    strip       INTEGER NOT NULL DEFAULT 0 CHECK (strip IN (0, 1)),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (hostname, path_prefix)
);

-- +goose StatementBegin
CREATE TRIGGER ingress_hostnames_cascade AFTER DELETE ON ingress_hostnames
WHEN old.stack_id IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM stacks WHERE id = old.stack_id)
 AND EXISTS (SELECT 1 FROM environments WHERE id = old.env_id)
BEGIN
    INSERT INTO ingress_env (env_id, config_version, updated_at) VALUES (old.env_id, 1, 0)
    ON CONFLICT (env_id) DO UPDATE SET config_version = ingress_env.config_version + 1;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER ingress_hostnames_cascade;
DROP TABLE ingress_routes;
DROP INDEX ingress_hostnames_domain;
DROP INDEX ingress_hostnames_stack;
DROP INDEX ingress_hostnames_env;
DROP TABLE ingress_hostnames;
DROP TABLE ingress_reserved;
DROP TABLE ingress_domains;
DROP TABLE ingress_env;
