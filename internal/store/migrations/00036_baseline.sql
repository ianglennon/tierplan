-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The schema of v0.1.0. The migrations written during development were squashed
-- into this one before the first release. It keeps the number of the last of them,
-- so the next migration is 00037.

-- +goose Up
CREATE TABLE app_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- The audit log is an HMAC chain (D-40): each row's hmac covers prev_hmac plus
-- the row's content, keyed from the master key identified by key_id. seq, not
-- ts, orders the chain, because two entries can share a nanosecond.
-- details is a JSON object whose values are all strings.
CREATE TABLE audit_log (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    id          TEXT NOT NULL UNIQUE,
    ts          INTEGER NOT NULL,
    actor_type  TEXT NOT NULL,
    actor_id    TEXT NOT NULL,
    actor_name  TEXT NOT NULL,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id   TEXT NOT NULL,
    env_id      TEXT,
    result      TEXT NOT NULL,
    reason      TEXT NOT NULL,
    ip          TEXT NOT NULL,
    user_agent  TEXT NOT NULL,
    details     TEXT NOT NULL,
    key_id      TEXT NOT NULL,
    prev_hmac   BLOB NOT NULL,
    hmac        BLOB NOT NULL
);

-- The policy model (D-45): roles hold permissions, assignments bind a role to a
-- principal. A NULL env_id means every environment. Phase 3 adds teams,
-- Operator and Viewer roles and per-environment assignments on these tables.
-- Permission names are validated against the Go registry, not by a foreign key,
-- so the registry can add a permission without a migration.
CREATE TABLE roles (
    id      TEXT PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    builtin INTEGER NOT NULL
);

CREATE TABLE role_assignments (
    id             TEXT PRIMARY KEY,
    principal_type TEXT NOT NULL,
    principal_id   TEXT NOT NULL,
    role_id        TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    env_id         TEXT,
    created_at     INTEGER NOT NULL
);

-- Local users (AUTH-01, D-29, D-35). Usernames are stored lowercase so the
-- UNIQUE constraint is case-insensitive. failed_count and locked_until carry the
-- per-account lockout state that plan 12 enforces; disabled is the soft off
-- switch. Timestamps are UTC unix nanoseconds.
CREATE TABLE users (
    id                  TEXT PRIMARY KEY,
    username            TEXT NOT NULL UNIQUE,
    password_hash       TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    password_changed_at INTEGER NOT NULL,
    failed_count        INTEGER NOT NULL DEFAULT 0,
    locked_until        INTEGER,
    disabled            INTEGER NOT NULL DEFAULT 0
, instance_admin INTEGER NOT NULL DEFAULT 0, display_name TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT '', deleted_at INTEGER, last_login_at INTEGER);

-- Server-side sessions (AUTH-02, D-32). The primary key is the SHA-256 of the
-- opaque 256-bit session ID, so a copy of the database never contains a usable
-- cookie value. absolute_expires_at is set for "remember me" sessions only;
-- the others end by idle timeout from last_seen_at (D-31). Timestamps are UTC
-- unix nanoseconds.
CREATE TABLE sessions (
    id_hash             BLOB PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at          INTEGER NOT NULL,
    last_seen_at        INTEGER NOT NULL,
    absolute_expires_at INTEGER,
    remember            INTEGER NOT NULL,
    ip                  TEXT NOT NULL,
    user_agent          TEXT NOT NULL
, public_id TEXT);

-- Managed Docker environments (ENV-04, D-19). Phase 1 knows one kind, the
-- Docker socket on the server's own host, and it exists only after an admin
-- opts in: nothing is inserted at startup. Phase 2 widens the kind check for
-- agent environments. The partial unique index is what makes two concurrent
-- opt-ins create one environment: the loser's insert fails and the service
-- maps that to 409 local_environment_exists. Timestamps are UTC unix
-- nanoseconds.
CREATE TABLE "environments" (
    id                 TEXT PRIMARY KEY,
    kind               TEXT NOT NULL CHECK (kind IN ('local','agent')),
    name               TEXT NOT NULL,
    socket_path        TEXT NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL DEFAULT 1,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    env_type           TEXT CHECK (env_type IN ('swarm','standalone')),
    swarm_cluster_id   TEXT,
    engine_id          TEXT,
    cluster_changed_at INTEGER,
    auto_update        INTEGER NOT NULL DEFAULT 1
, pending_cluster_id TEXT, name_auto INTEGER NOT NULL DEFAULT 0, local_node_id TEXT, local_socket_path TEXT, restart_loop_failures INTEGER NOT NULL DEFAULT 3
    CHECK (restart_loop_failures BETWEEN 2 AND 50), restart_loop_window_minutes INTEGER NOT NULL DEFAULT 10
    CHECK (restart_loop_window_minutes BETWEEN 1 AND 120), small_subnets INTEGER NOT NULL DEFAULT 0
    CHECK (small_subnets IN (0, 1)), small_subnet_mask INTEGER NOT NULL DEFAULT 26
    CHECK (small_subnet_mask BETWEEN 25 AND 28), address_pools TEXT NOT NULL DEFAULT '[]', address_settings_version INTEGER NOT NULL DEFAULT 0, address_settings_by TEXT NOT NULL DEFAULT '');

-- One join token per environment, usable many times (D-01). Only the SHA-256 of
-- its secret part is stored, so a stolen database cannot enrol a host (D-02).
-- Rotation inserts a new generation and marks the previous one superseded.
CREATE TABLE env_join_tokens (
    id            TEXT PRIMARY KEY,
    env_id        TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    token_hash    BLOB NOT NULL,
    generation    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    created_by    TEXT NOT NULL,
    superseded_at INTEGER,
    disabled_at   INTEGER
);

-- One issuing CA per environment (D-07). key_sealed is a vault envelope
-- v1.<keyID>.<ciphertext> sealed with the context agent-ca/v1/<envId>, so it
-- carries its key ID (D-56) and cannot be opened in another environment's row.
-- The certificate is public.
CREATE TABLE env_cas (
    env_id     TEXT PRIMARY KEY REFERENCES environments(id) ON DELETE CASCADE,
    cert_pem   TEXT NOT NULL,
    key_sealed TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

-- Every enrolled node (D-08). Public keys, fingerprints and serials are not
-- secret and are stored as they are (D-55).
CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    env_id           TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    role             TEXT NOT NULL,
    engine_id        TEXT NOT NULL,
    swarm_node_id    TEXT,
    public_key       BLOB NOT NULL,
    cert_serial      TEXT NOT NULL,
    cert_fingerprint TEXT NOT NULL,
    cert_not_after   INTEGER NOT NULL,
    agent_version    TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('active','revoked')),
    created_at       INTEGER NOT NULL,
    last_seen_at     INTEGER,
    revoked_at       INTEGER,
    revoked_by       TEXT,
    revoke_reason    TEXT
);

-- The environments a user used last (ENV-06, D-31), kept on the server so the
-- choice follows the user to another browser or device. One row per user and
-- environment, holding when it was last used as UTC unix nanoseconds; the newest
-- three per user are kept. A removed user or environment takes its rows with it.
CREATE TABLE user_recent_environments (
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    env_id  TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    used_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, env_id)
);

-- The identities of hosts an admin revoked (ENV-05, D-08, D-09). A revoke blocks the
-- engine ID and, on a swarm, the swarm node ID of the agent's host, so the host
-- cannot enrol again with a join token that is still valid. Only an unblock deletes
-- the rows. agent_id is the agent whose revoke made the block, so an unblock of
-- that agent lifts exactly its blocks. Removing the environment removes its blocks.
CREATE TABLE agent_blocks (
    env_id     TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('engine_id','swarm_node_id')),
    identity   TEXT NOT NULL,
    agent_id   TEXT,
    created_at INTEGER NOT NULL,
    created_by TEXT NOT NULL,
    PRIMARY KEY (env_id, kind, identity)
);

-- Dangerous permissions an Operator assignment carries on top of its role.
CREATE TABLE assignment_extras (
    assignment_id TEXT NOT NULL REFERENCES role_assignments (id) ON DELETE CASCADE,
    permission    TEXT NOT NULL,
    PRIMARY KEY (assignment_id, permission)
);

-- Teams are a second kind of principal: an assignment with principal_type 'team'
-- reaches every member.
CREATE TABLE teams (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE team_members (
    team_id    TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (team_id, user_id)
);

-- One live invite or reset link per user; only the hash of the token is stored.
CREATE TABLE user_links (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('invite', 'reset')),
    token_hash BLOB NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    created_by TEXT NOT NULL
);

-- Resources a user created that keep running on their authority (webhooks, git
-- syncs and the like). They pause when the owner loses the permission.
CREATE TABLE owned_resources (
    kind            TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    env_id          TEXT NOT NULL,
    name            TEXT NOT NULL,
    owner_id        TEXT NOT NULL REFERENCES users (id),
    state           TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'paused_lacks_permission', 'paused_disabled', 'paused_deleted')),
    needs_env_admin INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (kind, resource_id)
);

-- The scheduled backup: a single row (id is always 1).
CREATE TABLE backup_schedule (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    enabled           INTEGER NOT NULL DEFAULT 0,
    frequency         TEXT NOT NULL DEFAULT 'daily' CHECK (frequency IN ('daily', 'weekly')),
    weekday           INTEGER NOT NULL DEFAULT 0 CHECK (weekday BETWEEN 0 AND 6),
    time_utc          TEXT NOT NULL DEFAULT '03:00',
    keep              INTEGER NOT NULL DEFAULT 7 CHECK (keep BETWEEN 1 AND 365),
    passphrase_sealed TEXT NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL DEFAULT 0,
    updated_by        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE backup_runs (
    id          TEXT PRIMARY KEY,
    source      TEXT NOT NULL CHECK (source IN ('schedule', 'cli', 'ui')),
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    ok          INTEGER NOT NULL DEFAULT 0,
    file_name   TEXT NOT NULL DEFAULT '',
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);

-- How each user likes to open a shell in an image (OBS-03, D-26): the shell, the
-- custom command, the user to run as and the working directory of their last
-- successful open, keyed by user and image reference. The server writes a row when an
-- open succeeds and the launcher reads it back, so the choice follows the user to
-- another browser or device. Nothing here is a secret. At most 200 rows are kept per
-- user, the oldest trimmed on write. A removed user takes their rows with them.
-- Timestamps are UTC unix nanoseconds.
CREATE TABLE user_shell_prefs (
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    image       TEXT NOT NULL,
    shell       TEXT NOT NULL CHECK (shell IN ('auto', 'bash', 'sh', 'ash', 'custom')),
    command     TEXT NOT NULL DEFAULT '',
    run_as      TEXT NOT NULL DEFAULT '',
    working_dir TEXT NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, image)
);

-- Registry credentials (IMG-01, D-01 to D-05): the sign-in TierPlan uses when it pulls an
-- image or lists a repository's tags. A row with a NULL env_id is an instance credential,
-- one with an env_id belongs to that environment only and goes with it. A credential
-- applies to a registry host plus an optional namespace prefix (host 'ghcr.io', prefix
-- 'acme'); an empty prefix means the whole registry.
--
-- secret_sealed is a vault envelope sealed under the context registry-cred/v1/<id>, so a
-- value copied into another row does not open. It is the only place the secret lives and
-- it is never returned by the API. secret_last4 is what the page may show of it: the last
-- four characters, or nothing for a short secret. hub_fallback marks the Docker Hub
-- credential that serves every docker.io pull no more specific credential matches.
-- last_test_* and last_failure_* are written by later plans; they hold no secret.
-- Timestamps are UTC unix nanoseconds.
--
-- Uniqueness is per layer, so each rule is two partial indexes: one for the instance rows
-- (env_id IS NULL) and one for the rows of an environment.
CREATE TABLE registry_credentials (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT REFERENCES environments (id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    type               TEXT NOT NULL CHECK (type IN ('dockerhub', 'ghcr', 'gitlab', 'generic')),
    host               TEXT NOT NULL,
    prefix             TEXT NOT NULL DEFAULT '',
    username           TEXT NOT NULL,
    secret_sealed      TEXT NOT NULL,
    secret_last4       TEXT NOT NULL DEFAULT '',
    hub_fallback       INTEGER NOT NULL DEFAULT 0,
    ca_pem             TEXT NOT NULL DEFAULT '',
    insecure           INTEGER NOT NULL DEFAULT 0,
    expires_at         INTEGER,
    last_test_at       INTEGER,
    last_test_ok       INTEGER,
    last_test_detail   TEXT NOT NULL DEFAULT '',
    last_failure_at    INTEGER,
    last_failure_code  TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    created_by         TEXT NOT NULL
);

-- A flagged target a user has muted: the flag stays on its row, but it raises no bell
-- entry or dashboard count. A mute is keyed by environment, kind and target (the service
-- or container ID), so it follows the target and goes with its environment. name is the
-- target's name when it was muted, for the list of mutes once the target is gone.
-- muted_by is the user's ID. Timestamps are UTC unix nanoseconds.
CREATE TABLE restart_loop_mutes (
    env_id    TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    kind      TEXT NOT NULL CHECK (kind IN ('service', 'container')),
    target_id TEXT NOT NULL,
    name      TEXT NOT NULL DEFAULT '',
    muted_by  TEXT NOT NULL,
    muted_at  INTEGER NOT NULL,
    PRIMARY KEY (env_id, kind, target_id)
);

-- One row per stack TierPlan manages. target is fixed at creation from the environment kind. source_kind
-- is the source abstraction: inline now, adopted (Phase 8) and git (Phase 9) later; source_ref names the
-- adopted project or the git source. stopped is 1 while the stack is stopped (Phase 7, STK-11).
CREATE TABLE stacks (
    id          TEXT PRIMARY KEY,
    env_id      TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    target      TEXT NOT NULL CHECK (target IN ('swarm', 'standalone')),
    source_kind TEXT NOT NULL DEFAULT 'inline' CHECK (source_kind IN ('inline', 'adopted', 'git')),
    source_ref  TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    stopped     INTEGER NOT NULL DEFAULT 0, adoption TEXT NOT NULL DEFAULT '{}', janitor_excluded INTEGER NOT NULL DEFAULT 0,
    UNIQUE (env_id, name)
);

-- A stack's definition at a moment: exactly one draft per stack (what the editor saves) and any number of
-- revisions (a copy of the draft taken when a deploy starts). version counts saves of the draft, for the
-- optimistic check. seq numbers revisions per stack; base_seq is the revision a draft started from. outcome
-- marks a revision pending until its deploy ends, then good or failed. rendered and pins are filled when a
-- revision is deployed: the rendered spec and the image digests it ran with. Phase 7 never prunes revisions;
-- Phase 8 owns pruning and rollback to any revision.
CREATE TABLE stack_snapshots (
    id         TEXT PRIMARY KEY,
    stack_id   TEXT NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('draft', 'revision')),
    seq        INTEGER NOT NULL DEFAULT 0,
    base_seq   INTEGER NOT NULL DEFAULT 0,
    version    INTEGER NOT NULL DEFAULT 1,
    outcome    TEXT NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'good', 'failed')),
    rendered   TEXT NOT NULL DEFAULT '',
    pins       TEXT NOT NULL DEFAULT '{}',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
, origin TEXT NOT NULL DEFAULT 'deploy'
    CHECK (origin IN ('deploy','update','rollback','adopted')), restored_from INTEGER NOT NULL DEFAULT 0, platforms TEXT NOT NULL DEFAULT '{}', git_commit TEXT NOT NULL DEFAULT '', git_tree TEXT NOT NULL DEFAULT '', git_source TEXT NOT NULL DEFAULT '{}', compose_files TEXT NOT NULL DEFAULT '[]', sync_trigger TEXT NOT NULL DEFAULT ''
    CHECK (sync_trigger IN ('', 'poll', 'webhook')), reverted_drift TEXT NOT NULL DEFAULT '[]', subnets TEXT NOT NULL DEFAULT '{}');

-- The files of a snapshot (compose.yaml and the extra files beside it). A plain file keeps its text in
-- content; a secret file keeps a vault envelope in content_sealed, sealed under stack-file/v1/<snapshot>/<path>.
CREATE TABLE stack_snapshot_files (
    snapshot_id    TEXT NOT NULL REFERENCES stack_snapshots (id) ON DELETE CASCADE,
    path           TEXT NOT NULL,
    content        TEXT NOT NULL DEFAULT '',
    content_sealed TEXT NOT NULL DEFAULT '',
    secret         INTEGER NOT NULL DEFAULT 0, origin TEXT NOT NULL DEFAULT ''
    CHECK (origin IN ('', 'repo', 'overlay')),
    PRIMARY KEY (snapshot_id, path)
);

-- The variables of a snapshot. A secret variable keeps a vault envelope in value_sealed, sealed under
-- stack-var/v1/<snapshot>/<name>.
CREATE TABLE stack_snapshot_vars (
    snapshot_id  TEXT NOT NULL REFERENCES stack_snapshots (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    value        TEXT NOT NULL DEFAULT '',
    value_sealed TEXT NOT NULL DEFAULT '',
    secret       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (snapshot_id, name)
);

-- One row per operation on a stack (a deploy, an update of images, a rollback, a start, a stop, a remove).
-- job_id is the jobs hub job that ran it; failed_service and code say where and why a failed one stopped.
CREATE TABLE stack_deploys (
    id             TEXT PRIMARY KEY,
    stack_id       TEXT NOT NULL REFERENCES stacks (id) ON DELETE CASCADE,
    env_id         TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('deploy', 'update', 'rollback', 'start', 'stop', 'remove')),
    state          TEXT NOT NULL CHECK (state IN ('running', 'done', 'failed', 'cancelled', 'interrupted')),
    revision_seq   INTEGER NOT NULL DEFAULT 0,
    job_id         TEXT NOT NULL DEFAULT '',
    started_by     TEXT NOT NULL,
    started_at     INTEGER NOT NULL,
    ended_at       INTEGER NOT NULL DEFAULT 0,
    failed_service TEXT NOT NULL DEFAULT '',
    code           TEXT NOT NULL DEFAULT ''
);

-- Git connections (GIT-01, Phase 9 D-01, D-02): the sign-in TierPlan uses to read a repository,
-- and the repository itself, as two separate objects. A credential is an HTTPS token (username
-- and token) or an SSH deploy key. A repository is an address, an optional credential picked
-- explicitly (there is no matching by address) and an optional custom certificate authority.
-- Many repositories can share one credential.
--
-- A row with a NULL env_id is an instance row, one with an env_id belongs to that environment
-- only and goes with it. A repository of an environment may use a credential of its own
-- environment or of the instance; an instance repository only an instance credential. The
-- foreign key cannot say that, so the store checks it (credential_scope) and the key only
-- stops a credential that repositories still use from being deleted (ON DELETE NO ACTION).
--
-- secret_sealed is a vault envelope sealed under the context git-cred/v1/<id>, so a value
-- copied into another row does not open. It is the only place the token or the private key
-- lives and no query but GetGitCredentialSecret selects it. secret_last4 is what the page may
-- show of an HTTPS token; public_key and fingerprint are what it shows of an SSH key. The
-- custom CA is public material and lives on the repository, not the credential. host_key*
-- belong to the SSH plan. last_* columns hold no secret. Timestamps are UTC unix nanoseconds.
--
-- Uniqueness of a name is per layer, so each rule is two partial indexes.
CREATE TABLE git_credentials (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT REFERENCES environments (id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    type               TEXT NOT NULL CHECK (type IN ('https', 'ssh')),
    username           TEXT NOT NULL DEFAULT '',
    secret_sealed      TEXT NOT NULL,
    secret_last4       TEXT NOT NULL DEFAULT '',
    public_key         TEXT NOT NULL DEFAULT '',
    fingerprint        TEXT NOT NULL DEFAULT '',
    last_use_at        INTEGER,
    last_failure_at    INTEGER,
    last_failure_code  TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    created_by         TEXT NOT NULL
);

CREATE TABLE git_repos (
    id                    TEXT PRIMARY KEY,
    env_id                TEXT REFERENCES environments (id) ON DELETE CASCADE,
    name                  TEXT NOT NULL,
    url                   TEXT NOT NULL,
    credential_id         TEXT REFERENCES git_credentials (id) ON DELETE NO ACTION,
    ca_pem                TEXT NOT NULL DEFAULT '',
    host_key              TEXT NOT NULL DEFAULT '',
    host_key_fp           TEXT NOT NULL DEFAULT '',
    host_key_trusted_by   TEXT NOT NULL DEFAULT '',
    host_key_trusted_at   INTEGER,
    last_test_at          INTEGER,
    last_test_ok          INTEGER,
    last_test_code        TEXT NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    created_by            TEXT NOT NULL
);

-- Git-backed stacks (Phase 9, GIT-02, D-06, D-14). A stack with source kind git has one stack_git row: the
-- repository, the ref it tracks, the folder, the ordered compose files and the settings of keeping it in
-- sync. A stack_git_state row is the derived answer of the last check against the repository; it is rebuilt
-- by a check and is not part of a backup.
--
-- repo_id is ON DELETE NO ACTION: a repository that a stack uses cannot be deleted (the store answers
-- repo_in_use first, the key is the backstop). Deleting a stack or its environment removes the rows.
-- compose_files is a JSON array of paths relative to the folder, in merge order. webhook_token_hash is the
-- SHA-256 of the routing token in the webhook address; webhook_secret_sealed is a vault envelope sealed
-- under git-webhook/v1/<stack_id>. Timestamps are UTC unix nanoseconds.
CREATE TABLE stack_git (
    stack_id                  TEXT PRIMARY KEY REFERENCES stacks (id) ON DELETE CASCADE,
    repo_id                   TEXT NOT NULL REFERENCES git_repos (id) ON DELETE NO ACTION,
    ref_kind                  TEXT NOT NULL CHECK (ref_kind IN ('branch', 'tag', 'commit')),
    ref                       TEXT NOT NULL,
    folder                    TEXT NOT NULL DEFAULT '',
    compose_files             TEXT NOT NULL,
    poll_enabled              INTEGER NOT NULL DEFAULT 1,
    poll_interval_s           INTEGER NOT NULL DEFAULT 300 CHECK (poll_interval_s BETWEEN 60 AND 86400),
    auto_deploy               INTEGER NOT NULL DEFAULT 0,
    auto_deploy_paused_reason TEXT NOT NULL DEFAULT '' CHECK (auto_deploy_paused_reason IN ('', 'rollback', 'owner')),
    auto_deploy_paused_seq    INTEGER NOT NULL DEFAULT 0,
    webhook_flavour           TEXT NOT NULL DEFAULT '' CHECK (webhook_flavour IN ('', 'github', 'gitlab', 'gitea', 'generic')),
    webhook_placement         TEXT NOT NULL DEFAULT '' CHECK (webhook_placement IN ('', 'path', 'header')),
    webhook_token_hash        TEXT NOT NULL DEFAULT '',
    webhook_secret_sealed     TEXT NOT NULL DEFAULT '',
    webhook_secret_last4      TEXT NOT NULL DEFAULT '',
    webhook_created_by        TEXT NOT NULL DEFAULT '',
    webhook_last_at           INTEGER,
    webhook_last_result       TEXT NOT NULL DEFAULT '',
    pending_convert           INTEGER NOT NULL DEFAULT 0,
    created_at                INTEGER NOT NULL,
    updated_at                INTEGER NOT NULL
);

CREATE TABLE stack_git_state (
    stack_id             TEXT PRIMARY KEY REFERENCES stacks (id) ON DELETE CASCADE,
    deployed_commit      TEXT NOT NULL DEFAULT '',
    deployed_tree        TEXT NOT NULL DEFAULT '',
    head_commit          TEXT NOT NULL DEFAULT '',
    head_tree            TEXT NOT NULL DEFAULT '',
    behind               INTEGER NOT NULL DEFAULT 0,
    status               TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'in_sync', 'behind', 'error')),
    reason_code          TEXT NOT NULL DEFAULT '',
    reason_detail        TEXT NOT NULL DEFAULT '',
    checked_at           INTEGER,
    error_since          INTEGER,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    pending_commit       TEXT NOT NULL DEFAULT '',
    next_check_at        INTEGER
, event_kind TEXT NOT NULL DEFAULT ''
    CHECK (event_kind IN ('', 'autodeploy_skipped', 'autodeploy_reverted', 'autodeploy_paused')), event_detail TEXT NOT NULL DEFAULT '', event_at INTEGER);

-- When TierPlan first saw each network and last saw it in use (NET-04, D-09). Docker keeps no
-- such fact, and a network's creation time alone would flag a long-lived network the moment it
-- is freed, so the orphan check measures its 24-hour grace period from the later of the two.
-- node_id is '' for a swarm-scoped network, which is one network for the whole cluster, and
-- the node's ID for a local one, since another node's network can carry the same ID. Timestamps
-- are UTC unix nanoseconds; last_in_use_at is 0 for a network never seen in use. The Phase 11
-- janitor reads this table too, and a backup carries it.
CREATE TABLE network_seen (
    env_id         TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id        TEXT NOT NULL DEFAULT '',
    network_id     TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    first_seen_at  INTEGER NOT NULL,
    last_in_use_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (env_id, node_id, network_id)
);

-- Every run of the janitor, from a person's Clean now and from the schedule (JAN-10, D-16). The row
-- is written before the run removes anything and finalised when it ends, so a run that was cut
-- short is still on record. A backup carries it. Timestamps are UTC unix nanoseconds.
--   trigger      what started the run: the schedule, or a person pressing Clean now.
--   actor        who started it: the user's ID, or the name of the system for a scheduled run.
--   state        waiting and running while it works; completed, partial (an item could not be
--                removed), skipped (it did not run), failed (it could not run) once it has ended.
--   reason       why a run was skipped or failed, '' when it ran: a word the UI maps to its copy.
--   job_id       the job in the jobs hub that does the removing, '' until it exists.
--   started_at   when the run was started; finished_at is 0 while it is not over.
--   removed_*    how many containers, images and networks this run removed.
--   kept         how many selected items the run left alone because they were in use, kept by a
--                label or no longer stale; failed how many could not be removed; skipped_offline
--                how many were not sent because their node was not connected.
--   est_bytes    the bytes the removals freed that were known, -1 when no size was known.
CREATE TABLE janitor_runs (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    trigger            TEXT NOT NULL CHECK (trigger IN ('schedule', 'manual')),
    actor              TEXT NOT NULL,
    state              TEXT NOT NULL CHECK (state IN ('waiting', 'running', 'completed', 'partial', 'skipped', 'failed')),
    reason             TEXT NOT NULL DEFAULT '',
    job_id             TEXT NOT NULL DEFAULT '',
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER NOT NULL DEFAULT 0,
    removed_containers INTEGER NOT NULL DEFAULT 0,
    removed_images     INTEGER NOT NULL DEFAULT 0,
    removed_networks   INTEGER NOT NULL DEFAULT 0,
    kept               INTEGER NOT NULL DEFAULT 0,
    failed             INTEGER NOT NULL DEFAULT 0,
    skipped_offline    INTEGER NOT NULL DEFAULT 0,
    est_bytes          INTEGER NOT NULL DEFAULT -1
);

-- Each item a run attempted, in the order it attempted them. Removing a run removes its items.
--   seq            1 for the first item the run attempted, counting up; with run_id the key.
--   type           the kind of object.
--   node_id        the node the object is on, node_name its name; '' for an object of the whole swarm.
--   ref            the engine's ID of the object, name its name when the run read it.
--   reason         the code of why the planner selected it (the same words the preview uses) and
--                  reason_detail the rest of the reason as a JSON object (service, slot, rank, ...).
--   outcome        how the attempt ended.
--   code           a stable word for why it did not end as a removal, '' for a clean removal.
--   est_bytes      the bytes removing it frees, -1 when not known.
CREATE TABLE janitor_run_items (
    run_id        TEXT NOT NULL REFERENCES janitor_runs (id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('container', 'image', 'network')),
    node_id       TEXT NOT NULL DEFAULT '',
    node_name     TEXT NOT NULL DEFAULT '',
    ref           TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL,
    reason_detail TEXT NOT NULL DEFAULT '{}',
    outcome       TEXT NOT NULL CHECK (outcome IN ('removed', 'refused_in_use', 'skipped_protected', 'skipped_node_offline', 'error')),
    code          TEXT NOT NULL DEFAULT '',
    est_bytes     INTEGER NOT NULL DEFAULT -1,
    PRIMARY KEY (run_id, seq)
);

-- The crash evidence of a container the janitor removed whose exit code was not 0 (JAN-10, D-06):
-- why it stopped and the tail of its log, saved before the removal. It lives in its own table so
-- that listing a run's items never reads log text, it goes with its run when the run is pruned, and
-- a backup carries it. Timestamps are UTC unix nanoseconds, 0 when the engine reported none.
--   run_id, seq   the run and the item the evidence belongs to (the key of janitor_run_items; the
--                 evidence is saved before the item row is, so the key is not a foreign key).
--   exit_code     the container's exit code; error the engine's last start error, '' for none.
--   oom_killed    1 when the kernel killed the container for running out of memory.
--   image         the image reference the container was created with, image_id the image it ran and
--                 image_digest the repo@sha256 name of that image on its node, '' when unknown.
--   node          the name of the node it ran on.
--   log_text      the last lines of its log, masked and capped, one line after another; log_lines
--                 how many lines that is.
--   log_state     saved: the lines are in log_text. empty: the container wrote none. unavailable:
--                 its log driver cannot be read back. cap_reached: the run already holds the most
--                 evidence it may, so no log text was kept for this one.
CREATE TABLE janitor_evidence (
    run_id       TEXT NOT NULL REFERENCES janitor_runs (id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    exit_code    INTEGER NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    oom_killed   INTEGER NOT NULL DEFAULT 0,
    started_at   INTEGER NOT NULL DEFAULT 0,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    image        TEXT NOT NULL DEFAULT '',
    image_id     TEXT NOT NULL DEFAULT '',
    image_digest TEXT NOT NULL DEFAULT '',
    node         TEXT NOT NULL DEFAULT '',
    log_text     TEXT NOT NULL DEFAULT '',
    log_lines    INTEGER NOT NULL DEFAULT 0,
    log_state    TEXT NOT NULL DEFAULT 'saved' CHECK (log_state IN ('saved', 'empty', 'unavailable', 'cap_reached')),
    PRIMARY KEY (run_id, seq)
);

-- The instance defaults: always exactly one row, seeded with the policy of an environment nobody has
-- configured (D-01). The CHECK limits are the ones the settings page accepts.
--   enabled                    1 when the schedule runs the janitor.
--   schedule                   a five-field cron expression, read in the server's time zone.
--   schedule_preset            the name the page shows it by: hourly, every_6h, daily, weekly or custom.
--   keep_n                     how many of the newest exited task containers of each service slot are kept.
--   grace_hours                how long an object must have been stopped or idle before it can be removed.
--   remove_unused_networks     1 to also remove networks no stack ever owned (D-11).
--   remove_stopped_containers  1 to also remove stopped containers no stack or project ever owned (D-11).
--   retention_days             how long the run history is kept ...
--   retention_runs             ... or the newest this many runs of an environment, whichever keeps more.
--   version                    counts the saves, so a save made from a stale read is refused.
--   updated_by                 who saved it last; updated_at when (0 for the seeded row).
CREATE TABLE janitor_defaults (
    id                        INTEGER PRIMARY KEY CHECK (id = 1),
    enabled                   INTEGER NOT NULL,
    schedule                  TEXT NOT NULL,
    schedule_preset           TEXT NOT NULL,
    keep_n                    INTEGER NOT NULL CHECK (keep_n BETWEEN 0 AND 20),
    grace_hours               INTEGER NOT NULL CHECK (grace_hours BETWEEN 1 AND 720),
    remove_unused_networks    INTEGER NOT NULL,
    remove_stopped_containers INTEGER NOT NULL,
    retention_days            INTEGER NOT NULL CHECK (retention_days BETWEEN 7 AND 3650),
    retention_runs            INTEGER NOT NULL CHECK (retention_runs BETWEEN 10 AND 1000),
    version                   INTEGER NOT NULL DEFAULT 1,
    updated_by                TEXT NOT NULL DEFAULT '',
    updated_at                INTEGER NOT NULL DEFAULT 0
);

-- One environment's overrides of the defaults. A missing row and a row of NULLs both mean "follow
-- every default". The fields are those of janitor_defaults; schedule and schedule_preset are set
-- together or not at all.
--   version  0 while the environment has no row; each save counts one up, so a save made from a
--            stale read is refused.
CREATE TABLE janitor_env_policy (
    env_id                    TEXT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    enabled                   INTEGER,
    schedule                  TEXT,
    schedule_preset           TEXT,
    keep_n                    INTEGER CHECK (keep_n IS NULL OR keep_n BETWEEN 0 AND 20),
    grace_hours               INTEGER CHECK (grace_hours IS NULL OR grace_hours BETWEEN 1 AND 720),
    remove_unused_networks    INTEGER,
    remove_stopped_containers INTEGER,
    version                   INTEGER NOT NULL DEFAULT 0,
    updated_by                TEXT NOT NULL DEFAULT '',
    updated_at                INTEGER NOT NULL DEFAULT 0
);

-- One swarm node's overrides of its environment: whether the node is cleaned at all, and how many
-- exited containers of a service slot it keeps. A NULL field follows the environment. The node is
-- named by its swarm node ID, which stays the same when the node's agent is enrolled again.
CREATE TABLE janitor_node_policy (
    env_id     TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id    TEXT NOT NULL,
    enabled    INTEGER,
    keep_n     INTEGER CHECK (keep_n IS NULL OR keep_n BETWEEN 0 AND 20),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (env_id, node_id)
);

-- Everything a person asked the janitor, or the orphan check, never to remove or list: one table
-- for networks, containers and images (Phase 11, D-12). It generalises the Phase 10 network_keeps
-- table, whose rows are copied across below as kind 'network'. kind is network, container or
-- image. ref is what the keep names: the network's name (not its ID, so a network of the same
-- name that is made again stays kept, as in Phase 10), the container's ID, or the image's ID.
-- name is the label shown in the list (the network's name, a container's name, an image's
-- first tag). node_id is '' for a swarm-wide network and otherwise the ID of the node the object
-- lives on (envs.KeepNodeKey); an agent that is enrolled again gets a new node ID, so a keep on
-- a node-local object does not follow it there. kept_by is the user's ID. Timestamps are UTC
-- unix nanoseconds.
CREATE TABLE janitor_keeps (
    env_id  TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    kind    TEXT NOT NULL CHECK (kind IN ('network', 'container', 'image')),
    node_id TEXT NOT NULL DEFAULT '',
    ref     TEXT NOT NULL,
    name    TEXT NOT NULL DEFAULT '',
    kept_by TEXT NOT NULL,
    kept_at INTEGER NOT NULL,
    PRIMARY KEY (env_id, kind, node_id, ref)
);

-- When the janitor first saw each image without a name (Phase 11, D-07). Docker keeps no such fact: an
-- image's creation time is when it was built, which for an image that has just lost its tag to a newer
-- pull or build is usually long ago, so the grace period is measured from the later of the two. A row
-- is made the first time a snapshot finds the image dangling on its node and is deleted when the image
-- is gone or has a name again, so an image that is tagged and loses the tag again starts a new grace.
-- node_id is the ID of the node the image is on. first_seen_at is UTC unix nanoseconds.
CREATE TABLE image_dangling_seen (
    env_id        TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL,
    image_id      TEXT NOT NULL,
    first_seen_at INTEGER NOT NULL,
    PRIMARY KEY (env_id, node_id, image_id)
);

-- What a registry answered when TierPlan last asked it for the digest of a tag (Phase 12, D-04). A row
-- is the registry's answer, not an environment's: every environment that resolves the same credential
-- for a tag shares one row, and an environment with a credential of its own gets a row of its own
-- (D-19). Docker stays the source of truth: a row is matched to the digest a workload runs live, so
-- nothing here says whether anything is up to date. It holds no credential and no registry text, only
-- a mapped reason code. Timestamps are UTC unix nanoseconds.
--   host        the registry host, with the port when there is one (docker.io for Docker Hub).
--   repo        the repository path below the host (library/nginx).
--   tag         the tag that was asked about.
--   cred_id     the registry credential the check ran with, '' for an anonymous one.
--   digest      the digest the tag pointed at when the last check succeeded, '' before any did.
--   is_index    1 when that digest names a multi-platform index.
--   platforms   a JSON object of platform key (linux/amd64) to that platform's manifest digest.
--   checked_at  when the last check ran, successful or not; success_at when the last successful one did,
--               0 before any did.
--   error_code  why the last check failed, '' when it succeeded: auth, rate_limited, unreachable,
--               not_found, unsupported or other.
CREATE TABLE image_checks (
    host       TEXT NOT NULL,
    repo       TEXT NOT NULL,
    tag        TEXT NOT NULL,
    cred_id    TEXT NOT NULL DEFAULT '',
    digest     TEXT NOT NULL DEFAULT '',
    is_index   INTEGER NOT NULL DEFAULT 0,
    platforms  TEXT NOT NULL DEFAULT '{}',
    checked_at INTEGER NOT NULL DEFAULT 0,
    success_at INTEGER NOT NULL DEFAULT 0,
    error_code TEXT NOT NULL DEFAULT '' CHECK (error_code IN ('', 'auth', 'rate_limited', 'unreachable', 'not_found', 'unsupported', 'other')),
    PRIMARY KEY (host, repo, tag, cred_id)
);

-- The instance defaults: always exactly one row, seeded with the policy of an environment nobody has
-- configured: checked, every 6 hours (D-01).
--   enabled          1 when the schedule checks for updates.
--   schedule         a five-field cron expression, read in the server's time zone, firing no more often
--                    than every hour.
--   schedule_preset  the name the page shows it by: hourly, every_6h, daily, weekly or custom.
--   version          counts the saves, so a save made from a stale read is refused.
--   updated_by       who saved it last; updated_at when (0 for the seeded row).
CREATE TABLE image_update_defaults (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    enabled         INTEGER NOT NULL,
    schedule        TEXT NOT NULL,
    schedule_preset TEXT NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    updated_by      TEXT NOT NULL DEFAULT '',
    updated_at      INTEGER NOT NULL DEFAULT 0
);

-- One environment's overrides of the defaults. A missing row and a row of NULLs both mean "follow every
-- default". schedule and schedule_preset are set together or not at all.
--   enabled          NULL follows the default; 0 stops the scheduled checks of this environment (a person
--                    can still press Check now); 1 runs them.
--   schedule         NULL follows the default.
--   schedule_preset  the name of schedule; NULL together with schedule.
--   version          0 while the environment has no row; each save counts one up, so a save made from a
--                    stale read is refused.
--   updated_by       who saved it last; updated_at when.
CREATE TABLE image_update_env_policy (
    env_id          TEXT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    enabled         INTEGER,
    schedule        TEXT,
    schedule_preset TEXT,
    version         INTEGER NOT NULL DEFAULT 0,
    updated_by      TEXT NOT NULL DEFAULT '',
    updated_at      INTEGER NOT NULL DEFAULT 0
);

-- What a person asked TierPlan to stop offering image updates for (Phase 12, D-15, D-16). A skip passes over
-- one remote version of one workload; an ignore hides every update of an image or of a workload. Nothing
-- is ignored across environments: every row belongs to one environment and goes with it. Timestamps are
-- UTC unix nanoseconds.
--   id      the row's own ID.
--   env_id  the environment the row applies to.
--   mode    skip (one remote version) or ignore (all updates).
--   scope   image (a repository and tag) or workload (one service or container). A skip is always scope
--           workload.
--   host, repo, tag
--           the image of an image-scoped row, as the registry check keys it; empty for a workload scope.
--   kind, name
--           the workload of a workload-scoped row: service or container, and its name; empty for an image
--           scope.
--   digest  the remote digest a skip passes over, so it stops applying when the tag points at another one;
--           empty for an ignore.
--   set_by  the user's ID; set_at when.
CREATE TABLE image_update_ignores (
    id     TEXT PRIMARY KEY,
    env_id TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    mode   TEXT NOT NULL CHECK (mode IN ('skip', 'ignore')),
    scope  TEXT NOT NULL CHECK (scope IN ('image', 'workload')),
    host   TEXT NOT NULL DEFAULT '',
    repo   TEXT NOT NULL DEFAULT '',
    tag    TEXT NOT NULL DEFAULT '',
    kind   TEXT NOT NULL DEFAULT '' CHECK (kind IN ('', 'service', 'container')),
    name   TEXT NOT NULL DEFAULT '',
    digest TEXT NOT NULL DEFAULT '',
    set_by TEXT NOT NULL,
    set_at INTEGER NOT NULL,
    UNIQUE (env_id, mode, scope, host, repo, tag, kind, name)
);

-- The record of every image update (Phase 12, IMG-06): a row written before anything touches an engine,
-- found again while it runs, and ended exactly once. Nothing here is a credential: registry auth is never
-- stored. Timestamps are UTC unix nanoseconds.
--   id            the run's own ID.
--   env_id        the environment the update ran in; the run goes with it.
--   bulk_id       ties the rows of one bulk update together (D-11); empty for a single update.
--   kind          service, container or stack: what was updated.
--   workload      the service, container or stack name; workload_id its engine ID, stack_id the stack's ID
--                 when the workload belongs to one. Both empty when not known.
--   items         JSON [{service, image, from_digest, to_digest}], one per service or container updated.
--   state         queued, running, done, failed, rolled_back, rollback_failed or refused (D-14).
--   reason        why a run failed, was refused or was rolled back; empty otherwise.
--   actor         the user's ID, or a system name for an automatic update.
--   job_id        the background job that carries the run; empty until it has one.
--   revision_seq  the revision (stack snapshot) sequence number the update wrote; rollback_seq the one a
--                 rollback restored. 0 when there is none.
--   old_container the ID of the container a container update replaced, recorded before it is stopped.
--   started_at    when the run was recorded; finished_at when it ended, 0 while it has not.
CREATE TABLE image_update_runs (
    id            TEXT PRIMARY KEY,
    env_id        TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    bulk_id       TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL CHECK (kind IN ('service', 'container', 'stack')),
    workload      TEXT NOT NULL,
    workload_id   TEXT NOT NULL DEFAULT '',
    stack_id      TEXT NOT NULL DEFAULT '',
    items         TEXT NOT NULL DEFAULT '[]',
    state         TEXT NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed', 'rolled_back', 'rollback_failed', 'refused')),
    reason        TEXT NOT NULL DEFAULT '',
    actor         TEXT NOT NULL,
    job_id        TEXT NOT NULL DEFAULT '',
    revision_seq  INTEGER NOT NULL DEFAULT 0,
    rollback_seq  INTEGER NOT NULL DEFAULT 0,
    old_container TEXT NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER NOT NULL DEFAULT 0
);

-- The remote digest an image update failed with, remembered per workload (Phase 12, IMG-07, D-14), so the
-- badge can say "Update failed, rolled back" for that exact digest and stop offering it until a newer one
-- appears. A workload has at most one row: a newer failure replaces it, and a later good update clears it.
-- Nothing here is a credential. Timestamps are UTC unix nanoseconds.
--   env_id           the environment the workload is in; the row goes with it.
--   kind             service or container: what kind of workload failed.
--   workload         the service or container name.
--   digest           the remote digest the update failed with.
--   reason           why it failed.
--   run_id           the update run that failed (image_update_runs.id).
--   rollback_failed  1 when restoring the previous image failed too, 0 when the rollback worked.
--   failed_at        when the failure was recorded.
CREATE TABLE image_update_failures (
    env_id          TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('service', 'container')),
    workload        TEXT NOT NULL,
    digest          TEXT NOT NULL,
    reason          TEXT NOT NULL,
    run_id          TEXT NOT NULL,
    rollback_failed INTEGER NOT NULL DEFAULT 0,
    failed_at       INTEGER NOT NULL,
    PRIMARY KEY (env_id, kind, workload)
);

CREATE INDEX audit_log_ts ON audit_log (ts);

CREATE INDEX audit_log_actor_name ON audit_log (actor_name);

CREATE INDEX audit_log_action ON audit_log (action);

CREATE INDEX role_assignments_principal
    ON role_assignments (principal_type, principal_id);

CREATE INDEX sessions_user_id ON sessions (user_id);

CREATE UNIQUE INDEX environments_one_local ON environments (kind) WHERE kind = 'local';

CREATE UNIQUE INDEX environments_cluster ON environments (swarm_cluster_id) WHERE swarm_cluster_id IS NOT NULL;

CREATE UNIQUE INDEX environments_engine ON environments (engine_id)
    WHERE kind = 'agent' AND env_type = 'standalone' AND engine_id IS NOT NULL;

CREATE INDEX env_join_tokens_env ON env_join_tokens (env_id, generation);

CREATE INDEX agents_env ON agents (env_id);

CREATE INDEX user_recent_environments_newest ON user_recent_environments (user_id, used_at DESC, env_id);

CREATE INDEX agent_blocks_agent ON agent_blocks (agent_id);

CREATE INDEX agents_engine ON agents (engine_id);

CREATE INDEX agents_swarm_node ON agents (swarm_node_id) WHERE swarm_node_id IS NOT NULL;

CREATE UNIQUE INDEX role_assignments_subject_scope
    ON role_assignments (principal_type, principal_id, COALESCE(env_id, ''));

CREATE UNIQUE INDEX teams_name ON teams (lower(name));

CREATE INDEX team_members_user ON team_members (user_id);

CREATE UNIQUE INDEX user_links_user ON user_links (user_id);

CREATE UNIQUE INDEX sessions_public_id ON sessions (public_id);

CREATE INDEX owned_resources_owner ON owned_resources (owner_id);

CREATE INDEX audit_log_env ON audit_log (env_id);

CREATE INDEX user_shell_prefs_newest ON user_shell_prefs (user_id, updated_at DESC, image);

CREATE UNIQUE INDEX registry_cred_host_prefix_inst ON registry_credentials (host, prefix) WHERE env_id IS NULL;

CREATE UNIQUE INDEX registry_cred_host_prefix_env ON registry_credentials (env_id, host, prefix) WHERE env_id IS NOT NULL;

CREATE UNIQUE INDEX registry_cred_name_inst ON registry_credentials (name) WHERE env_id IS NULL;

CREATE UNIQUE INDEX registry_cred_name_env ON registry_credentials (env_id, name) WHERE env_id IS NOT NULL;

CREATE UNIQUE INDEX registry_cred_hubfb_inst ON registry_credentials (host) WHERE env_id IS NULL AND hub_fallback = 1;

CREATE UNIQUE INDEX registry_cred_hubfb_env ON registry_credentials (env_id) WHERE env_id IS NOT NULL AND hub_fallback = 1;

CREATE UNIQUE INDEX stack_one_draft ON stack_snapshots (stack_id) WHERE kind = 'draft';

CREATE UNIQUE INDEX stack_revision_seq ON stack_snapshots (stack_id, seq) WHERE kind = 'revision';

CREATE INDEX stack_deploys_by_stack ON stack_deploys (stack_id, started_at);

CREATE UNIQUE INDEX git_cred_name_inst ON git_credentials (name) WHERE env_id IS NULL;

CREATE UNIQUE INDEX git_cred_name_env ON git_credentials (env_id, name) WHERE env_id IS NOT NULL;

CREATE UNIQUE INDEX git_repo_name_inst ON git_repos (name) WHERE env_id IS NULL;

CREATE UNIQUE INDEX git_repo_name_env ON git_repos (env_id, name) WHERE env_id IS NOT NULL;

CREATE INDEX git_repo_credential ON git_repos (credential_id) WHERE credential_id IS NOT NULL;

CREATE UNIQUE INDEX stack_git_webhook_token ON stack_git (webhook_token_hash) WHERE webhook_token_hash != '';

CREATE INDEX stack_git_repo ON stack_git (repo_id);

CREATE INDEX janitor_runs_env_started ON janitor_runs (env_id, started_at DESC, id);

CREATE INDEX image_update_runs_env_state ON image_update_runs (env_id, state);

INSERT INTO roles (id, name, builtin) VALUES ('admin', 'admin', 1);
INSERT INTO roles (id, name, builtin) VALUES ('operator', 'operator', 1);
INSERT INTO roles (id, name, builtin) VALUES ('viewer', 'viewer', 1);

INSERT INTO janitor_defaults (id, enabled, schedule, schedule_preset, keep_n, grace_hours, remove_unused_networks, remove_stopped_containers, retention_days, retention_runs, version, updated_by, updated_at) VALUES (1, 1, '0 3 * * *', 'daily', 2, 24, 0, 0, 90, 100, 1, '', 0);

INSERT INTO image_update_defaults (id, enabled, schedule, schedule_preset, version, updated_by, updated_at) VALUES (1, 1, '0 */6 * * *', 'every_6h', 1, '', 0);

-- +goose Down
DROP TABLE image_update_failures;
DROP TABLE image_update_runs;
DROP TABLE image_update_ignores;
DROP TABLE image_update_env_policy;
DROP TABLE image_update_defaults;
DROP TABLE image_checks;
DROP TABLE image_dangling_seen;
DROP TABLE janitor_keeps;
DROP TABLE janitor_node_policy;
DROP TABLE janitor_env_policy;
DROP TABLE janitor_defaults;
DROP TABLE janitor_evidence;
DROP TABLE janitor_run_items;
DROP TABLE janitor_runs;
DROP TABLE network_seen;
DROP TABLE stack_git_state;
DROP TABLE stack_git;
DROP TABLE git_repos;
DROP TABLE git_credentials;
DROP TABLE stack_deploys;
DROP TABLE stack_snapshot_vars;
DROP TABLE stack_snapshot_files;
DROP TABLE stack_snapshots;
DROP TABLE stacks;
DROP TABLE restart_loop_mutes;
DROP TABLE registry_credentials;
DROP TABLE user_shell_prefs;
DROP TABLE backup_runs;
DROP TABLE backup_schedule;
DROP TABLE owned_resources;
DROP TABLE user_links;
DROP TABLE team_members;
DROP TABLE teams;
DROP TABLE assignment_extras;
DROP TABLE agent_blocks;
DROP TABLE user_recent_environments;
DROP TABLE agents;
DROP TABLE env_cas;
DROP TABLE env_join_tokens;
DROP TABLE environments;
DROP TABLE sessions;
DROP TABLE users;
DROP TABLE role_assignments;
DROP TABLE roles;
DROP TABLE audit_log;
DROP TABLE settings;
DROP TABLE app_meta;
