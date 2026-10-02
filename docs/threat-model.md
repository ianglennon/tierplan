# Bayplan threat model (Phase 1)

This describes what Bayplan protects against in its first build (install, sign-in and a read-only view of the local Docker host), what it does not, and the risks that remain. Threat IDs (T-01-nn) refer to the plans in which each mitigation was designed and tested.

## The headline

**Bayplan server compromise == root on every managed host.**

Bayplan manages Docker through the Docker API, and the Docker API is root-equivalent. A process that can talk to `/var/run/docker.sock` can start a privileged container that mounts the host's root file system, read any file, and change any setting. So whoever controls Bayplan controls every host it is connected to, whatever the individual buttons in its interface allow.

Three consequences follow, and the README repeats them before the install command (T-01-116):

- Mounting the socket is a decision about the host, not about an application. Do it only on hosts you intend Bayplan to manage. Nothing is managed until an admin opts in after setup, and the opt-in screen says the same.
- Every admin account is a root account on the managed hosts. Treat admin passwords, sessions and the machine that runs the browser accordingly.
- The strength of Bayplan's defences matters less than its exposure. Do not publish port 7480 to the internet. Put it behind TLS and, if you can, behind a network boundary or VPN.

Later phases add remote hosts through agents, which widens the same statement to "every host with an agent".

## Trust boundaries

| Boundary | What crosses it | Main controls |
|----------|-----------------|---------------|
| Browser to API | Sign-in, sessions, every change the UI makes | Server-side sessions, CSRF token, Origin check, strict CSP, per-route permission policy |
| Internet to setup and sign-in | The setup token, passwords | Setup token gate, rate limits, account lockout, one error for every sign-in failure |
| Bayplan to the Docker socket | Every Docker API call | Read-only typed facade, `unix://` only, privilege drop to UID 1000 |
| The `/data` volume | The database, the master key, the setup token file, the TLS key | File modes 0600, ownership by UID 1000, key never logged, backups documented |
| Reverse proxy to Bayplan | Client addresses, scheme | Forwarded headers believed only from `BAYPLAN_TRUSTED_PROXIES` |
| The build pipeline | Source, dependencies, the image | Pinned actions, licence gate, minimum release age, digest-pinned base, signatures and SBOM |

## What Phase 1 builds in

**Docker access is narrow and read-only (T-01-64, T-01-65, T-01-66).** All Docker calls go through one package, `internal/dockerx`, which exports typed read methods only. A reflection test pins that method set and bans write families; a lint rule keeps the Docker client out of every other package. `DOCKER_HOST` accepts `unix://` paths only, and the environment never chooses the API version or TLS settings. Engines Bayplan does not support (older than Docker 24, rootless, Docker Desktop, Podman) are detected and refused with a reason rather than treated as supported (T-01-80). Phase 1 has no state-changing Docker endpoints, and the interface has no row actions (T-01-85).

**Default-deny authorisation (T-01-04, T-01-29, T-01-30, T-01-31, T-01-34).** Every route declares a requirement. Registering one without a requirement panics at startup, and a startup check walks the router to prove each route is guarded. The policy engine denies anything not explicitly granted, resolves the environment before the permission check, and writes an audit entry for every denial; if that write fails the request fails. Hiding a control in the UI is only convenience (T-01-58): the server decides.

**Sessions and CSRF (T-01-44, T-01-52, T-01-53, T-01-33, T-01-57).** Session IDs are 256 random bits and only their SHA-256 is stored. The cookie is `HttpOnly` and `SameSite=Lax`, and `Secure` with the `__Host-` prefix when the public URL is `https`. A new session is created at sign-in. Every state-changing request needs an `X-CSRF-Token` derived from the session with a key held in the vault, compared in constant time, and an `Origin` that matches the public URL. Only the cookie of the configured scheme is read: on an `https` instance the bare cookie is ignored, so one planted from a sibling host or over plain http cannot stand in for it (T-01-G07), and changing the public URL scheme signs browsers out once. Sessions end after 12 hours idle, remembered sessions after 30 days, and sign-out deletes the server-side row. Open event streams were authorised only when they opened, so they are re-checked on every 15-second heartbeat (session, user, environment and permission, without touching the session) and capped at one hour. Sign-out, a password change that ends other sessions, idle expiry, a disabled user, a removed or disabled environment or a withdrawn permission therefore ends a stream within a heartbeat, and the browser's reconnect gets a 401 (T-01-G08). The sign-in `next` parameter only follows same-origin paths.

**Sign-in hardening (T-01-50, T-01-51, T-01-42, T-01-43, T-01-45, T-01-60).** Passwords are argon2id (m=19456 KiB, t=2, p=1, per-hash salt), at least 12 characters, checked against an embedded common-password list and against the user name. Five failed sign-ins lock an account for 30 minutes, and there is a per-address limit. Unknown, disabled and wrong-password cases give one error and one status, and unknown users still pay the hashing cost. Hashing is limited to four at a time. Changing a password needs the current one, is throttled per user, ends other sessions and is audited.

**The setup token gate (T-01-36, T-01-38, T-01-39, T-01-40, T-01-41, T-01-46).** An unclaimed instance has a 128-bit token, compared in constant time, with a per-address limit, and every failure is audited with the address. Once an admin exists the setup endpoints answer 409. Completion re-checks the user count inside the write transaction, so two simultaneous attempts cannot both win. There is no development or configuration switch that skips the gate (T-01-09).

**Audit log (T-01-23, T-01-25, T-01-27, T-01-28, T-01-98, T-01-99).** Every entry is chained with an HMAC keyed from the master key, which is not stored in the database, so rows edited or deleted from the middle are detectable with `bayplan audit verify`. Appends happen in one immediate transaction, so concurrent writers cannot fork the chain, and a failed append fails the request instead of dropping the entry. Detail fields cannot carry secrets, and lengths are capped. Only admins can read the log, and exports leave out the chain internals. CSV exports neutralise formula-looking cells (T-01-97, T-01-103).

**Secrets at rest (T-01-15, T-01-24).** The master key is 32 random bytes, in `/data/bayplan.key` (mode 0600, written atomically) or in `BAYPLAN_ENCRYPTION_KEY` / `BAYPLAN_ENCRYPTION_KEY_FILE`. It is never logged, never in the database and never returned by an API; only derived subkeys are used. Settings are logged with secrets redacted.

**Running unprivileged (T-01-108, T-01-109, T-01-112).** The container starts as root only to fix the ownership of `/data` and join the socket's group, then becomes UID and GID 1000, in every subcommand, so the command-line tools never leave root-owned database files. The base image is distroless, with no shell. The self-signed TLS key is written 0600 under `/data/tls`.

**Browser hardening (T-01-03, T-01-05, T-01-06, T-01-113, T-01-114).** The Content-Security-Policy is `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; ...; frame-ancestors 'none'`, plus `nosniff` and `Referrer-Policy: no-referrer`. Fonts are bundled and nothing loads from another origin. The one concession is inline styles, which the toast library needs (T-01-63); scripts stay `'self'`. The end-to-end suite fails on any CSP violation and on any request to another origin. All strings from Docker, users or the audit log are rendered as text, never as HTML (T-01-48, T-01-59, T-01-81, T-01-84, T-01-91, T-01-101).

**Data safety (T-01-18, T-01-19, T-01-20, T-01-21, T-01-22, T-01-61, T-01-62).** Migrations are transactional and preceded by a snapshot with a free-space guard; a database from a newer schema refuses to start. A `/data` that is not a mount, or is tmpfs, is warned about, and the key-backup reminder is acknowledged in the audit log. All SQL is generated with bound parameters.

**Availability controls (T-01-17, T-01-54, T-01-71, T-01-79, T-01-87, T-01-90).** The HTTP server has header (10 s), read (30 s) and idle (120 s) timeouts, so a client that sends headers and withholds the body is cut off; the read timeout ends when a bodyless request is being served, so the events stream is not affected, and there is no write timeout. A locked-out sole admin has `bayplan user unlock`. Probes of a dead daemon back off, and a slow resource listing never holds up a probe, so a busy but healthy engine is not reported offline. The self-signed certificate is kept across container recreation, renewed before it expires, and never pinned with HSTS, so an upgrade cannot lock a browser out. Resource lists are capped and cached. Each principal gets at most eight event streams, and one daemon stream serves any number of viewers.

**Opt-in management (T-01-69, T-01-74).** The local host becomes an environment only through an explicit request by an admin, after the root-equivalence notice, and the change is audited.

**Supply chain (T-01-08, T-01-11, T-01-12, T-01-13, T-01-14, T-01-111).** Packages younger than 24 hours are refused by pnpm; the lockfile is committed and installs are frozen. A licence allowlist (Go on both architectures, and production npm packages) fails the build on anything else, and exceptions need a written reason. CI actions are pinned to full commit SHAs with read-only tokens and no secrets. Commits need a DCO sign-off. Release images use a digest-pinned base, keyless cosign signatures and an SPDX SBOM, and there is no `:latest` tag (T-01-76).

## Residual risks

These are known, accepted for Phase 1, and not hidden.

1. **Audit tail truncation is undetectable without an external anchor (T-01-26).** The HMAC chain proves that entries were not edited or removed from the middle. Someone who can write to the database can delete only the newest entries, which leaves a valid, shorter chain, and `bayplan audit verify` will still pass. Detecting that needs the head of the chain recorded somewhere an attacker with the volume cannot reach, such as a SIEM; streaming the log out is deferred.
2. **The setup token is visible to anyone who can read the container log or `/data` until setup completes (T-01-37).** It is printed and written to `/data/setup-token` (mode 0600, deleted at completion) by design, so that you can claim the instance without a prior secret. Anyone who has log or volume access already has host admin, but a shared log pipeline can widen that audience. Claim the instance as soon as it starts.
3. **Plain HTTP exposes credentials (T-01-110).** Bayplan serves HTTP unless you configure TLS or a TLS-terminating proxy, and it warns when it does. Passwords and session cookies then cross the network in clear text. First-class reverse-proxy support is planned for a later phase; this build supports `BAYPLAN_TRUSTED_PROXIES` and an `https://` public URL.
4. **The master key sits on the data volume, so a backup of the volume holds both the key and the data it protects.** Encryption at rest therefore defends against a copy of the database file alone, not against a copy of the whole volume. Keep backups protected, or supply the key through `BAYPLAN_ENCRYPTION_KEY_FILE` from a separate store and back it up separately. Losing the key loses the stored secrets and the ability to verify the audit log (T-01-119).
5. **Everything an admin can do, an attacker with an admin session can do (the headline).** Phase 1 has one built-in role, admin, and no second factor. Bayplan limits damage with short idle timeouts, lockout and an audit trail; it cannot make an admin session less powerful than the socket it holds.
6. **Single-instance by design.** There is one SQLite writer and no high availability. A compromised or lost instance is recovered from backups.
7. **Inline styles are allowed by the CSP (T-01-63).** A style-injection bug would be easier to exploit than under a fully strict policy. Script injection stays blocked.
8. **Audit growth from scanners (T-01-35) and large exports (T-01-100)** are accepted: every denied read is audited, retention bounds the size, and exports stream from a read connection in batches. Repeated anonymous refusals from one address (denied routes, setup attempts after setup, rate-limited sign-ins) are written individually up to a per-address budget, 20 at once and then 10 a minute, and the rest as one summary entry per minute with a count, so a flood cannot starve the single database writer and every refusal is still recorded. Failed sign-ins that reach the account checks and wrong setup tokens are always written one row each.
9. **The CI end-to-end job runs a privileged Docker-in-Docker container (T-01-115)** on an ephemeral hosted runner with no secrets.

## Reporting a problem

Report suspected vulnerabilities privately, as described in [SECURITY.md](../SECURITY.md).
