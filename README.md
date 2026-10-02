# Bayplan

Bayplan is an open-source, web-based manager for Docker: many Docker Swarm clusters and standalone Docker hosts, from one place. It aims to be easier to use than Portainer, with nothing held back behind a paid tier.

Licence: [AGPL-3.0-or-later](LICENSE). Anyone who runs a modified version as a network service must offer its users the source.

**Status: pre-release.** No version has been published yet; the first release will be v0.1.0, and breaking changes are allowed between 0.x minors. This build lets you install Bayplan, sign in, and look at the Docker host it runs on: a dashboard, containers, images, volumes and networks (read-only), a live event feed, an audit log and settings. Managing stacks and other hosts comes later.

## Before you install

> **Mounting `/var/run/docker.sock` gives Bayplan, and every admin who signs in to it, root-equivalent control of that host.** Anyone who can use the Docker API can start a privileged container and read or change anything on the machine. Only mount the socket on a host you intend Bayplan to manage, protect the admin accounts accordingly, and see [docs/threat-model.md](docs/threat-model.md) for what this means in practice.

Bayplan sends no telemetry and makes no outbound calls you did not configure. The web interface loads nothing from other origins (no CDN, no analytics, bundled fonts), and its Content-Security-Policy keeps it that way.

## Install

```sh
docker run -d --name bayplan --restart unless-stopped \
  -p 7480:7480 \
  -v bayplan-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/ianglennon/bayplan:edge
```

The same thing as a Compose file:

```yaml
services:
  bayplan:
    image: ghcr.io/ianglennon/bayplan:edge
    container_name: bayplan
    restart: unless-stopped
    ports:
      - "7480:7480"
    volumes:
      - bayplan-data:/data
      - /var/run/docker.sock:/var/run/docker.sock

volumes:
  bayplan-data:
```

Image tags:

- `:edge` is built from the `main` branch. Use it until v0.1.0 is released.
- From a release on, `X.Y.Z` is that exact version and `X.Y` follows the patch releases of one minor line. Use `:X.Y` (for example `:0.1`).
- There is **no `:latest` tag** while Bayplan is 0.x. Every 0.x minor may break the one before it, so a command you copy must name the line it was written for. The in-app "no Docker socket" screens show the same command, with the tag that matches the running build.

The image is published for `linux/amd64` and `linux/arm64`. It is based on distroless `static` (no shell, no package manager).

### How the container runs

The container starts as root, and only for a moment. Bayplan fixes the ownership of `/data` (a new bind mount is root-owned), joins the group that owns the Docker socket, and then drops itself to UID and GID 1000 before it opens the database or serves a request. The same drop happens for every `docker exec <container> bayplan ...` command. If the socket's group cannot be detected, set `BAYPLAN_DOCKER_GID` to the group ID to join instead.

## First run

1. Open `http://HOST:7480`. A new instance shows the setup wizard.
2. The wizard asks for the **setup token**. Every start of an unclaimed instance prints a new one to the container log, and writes it to `/data/setup-token`:

   ```sh
   docker logs bayplan
   docker exec bayplan bayplan setup-token
   ```

3. Choose the first admin's user name and a password (12 characters or more; common passwords and passwords that contain the user name are refused), then an instance name and the public URL. The wizard fills in the address you are using; if `BAYPLAN_PUBLIC_URL` is set, that value is shown read-only.
4. After setup, Bayplan asks whether to manage the Docker host it runs on. Nothing is managed until an admin says yes. Choosing yes is the step that gives Bayplan control of the host.
5. Later, sign in at the same address with the account you created. After five failed attempts an account locks for 30 minutes (see [Operations](#operations) to clear a lock).

Anyone who can read the container log or `/data` can read the setup token until setup is finished; the token file is deleted when setup completes. Claim a new instance promptly, and do not expose port 7480 to a network you do not trust before you have.

## Requirements

- Docker Engine **24 to 29**, rootful, on Linux (`amd64` or `arm64`). Bayplan checks the API version and tells you when an engine is older than Docker 24 (API 1.43).
- **Not supported:** rootless Docker, Docker Desktop and Podman. Bayplan detects each of them and says why it cannot manage the host.
- `DOCKER_HOST` is honoured for `unix://` paths only. `tcp://` and `ssh://` addresses are refused at startup.
- On a **Docker Swarm**, run Bayplan on a manager node and as exactly one replica (the database is SQLite, a single writer). A service example, not exercised in Phase 1:

  ```sh
  docker service create --name bayplan --replicas 1 \
    --constraint 'node.role==manager' \
    --constraint 'node.hostname==MANAGER-HOSTNAME' \
    --publish 7480:7480 \
    --mount type=volume,source=bayplan-data,target=/data \
    --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
    ghcr.io/ianglennon/bayplan:edge
  ```

  A named volume lives on one node, so pin the service to that node as well as to the managers.

## Configuration

Every setting is an environment variable, and most are also a command-line flag (`bayplan serve --help` lists them). A flag that is set wins over the environment, and the environment wins over the default. Secrets are not available as flags, so they stay out of the process list; they accept a `_FILE` variant that reads the value from a file, for Docker secrets. Setting both `X` and `X_FILE` is an error. An invalid value stops startup and the message names the variable.

| Variable | Default | `_FILE` | Description |
|----------|---------|---------|-------------|
| `BAYPLAN_LISTEN` | `:7480` | no | Address to listen on, as `host:port` or `:port`. |
| `BAYPLAN_DATA_DIR` | `/data` | no | Directory for the database, the encryption key and the TLS certificate. |
| `BAYPLAN_PUBLIC_URL` | unset | no | Externally visible base URL, for example `https://bayplan.example.com`. Locks the stored value (see [Public URL](#public-url)). |
| `BAYPLAN_ENCRYPTION_KEY` | generated | yes | Key for secrets at rest and the audit chain: 32 random bytes as standard base64. When unset, a key is generated in `/data/bayplan.key`. |
| `BAYPLAN_TRUSTED_PROXIES` | unset | no | Comma-separated IPs or CIDRs of reverse proxies whose forwarded headers are believed. |
| `BAYPLAN_LOGIN_MAX_FAILURES` | `5` | no | Failed sign-ins before an account locks. |
| `BAYPLAN_LOGIN_LOCKOUT` | `30m` | no | How long a locked account stays locked. |
| `BAYPLAN_SESSION_IDLE_TIMEOUT` | `12h` | no | Idle time after which a session ends. |
| `BAYPLAN_SESSION_REMEMBER_LIFETIME` | `720h` | no | Lifetime of a "keep me signed in" session. |
| `BAYPLAN_AUDIT_RETENTION_DAYS` | `365` | no | Days to keep audit entries; `0` keeps them forever. |
| `BAYPLAN_SKIP_PRE_MIGRATE_BACKUP` | `false` | no | Do not snapshot the database before a migration. |
| `BAYPLAN_TLS_CERT_FILE` | unset | no | Path to a TLS certificate in PEM form. Needs `BAYPLAN_TLS_KEY_FILE`. |
| `BAYPLAN_TLS_KEY_FILE` | unset | no | Path to the TLS private key in PEM form. Needs `BAYPLAN_TLS_CERT_FILE`. |
| `BAYPLAN_TLS_SELF_SIGNED` | `false` | no | Serve a generated self-signed certificate. Cannot be combined with the two file settings. |
| `BAYPLAN_DOCKER_GID` | detected | no | Group ID of the Docker socket, overriding detection. |
| `BAYPLAN_LOG_LEVEL` | `info` | no | `debug`, `info`, `warn` or `error`. Logs are JSON on standard error. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | no | Docker daemon address; `unix://` only. Pointing it at a different daemon still shows the "Bayplan (this instance)" badge on the one Bayplan runs in. |

Only `BAYPLAN_ENCRYPTION_KEY` accepts `_FILE` (`BAYPLAN_ENCRYPTION_KEY_FILE`). Bayplan logs every setting at startup with secrets shown as `[redacted]`.

### TLS

By default Bayplan serves plain HTTP. Credentials then cross the network in clear text; only do that on a network you trust, or put a TLS-terminating proxy in front. There are two ways to serve HTTPS from Bayplan itself (TLS 1.2 or newer):

- **Your certificate:** mount the files and set `BAYPLAN_TLS_CERT_FILE` and `BAYPLAN_TLS_KEY_FILE`. Both or neither. When you replace the files, Bayplan picks up the new pair within a minute, with no restart; a replacement that cannot be loaded is logged and the working certificate stays in use. Bayplan sends `Strict-Transport-Security` (HSTS) with your certificate.
- **Self-signed:** set `BAYPLAN_TLS_SELF_SIGNED=true`. Bayplan keeps an ECDSA P-256 certificate and key in `/data/tls` (key mode 0600). It covers `localhost`, the loopback addresses, the machine's host name and the host of the public URL. A host name that is a container ID (12 or 64 hex characters, which Docker assigns and changes on every recreation) is left out, so recreating or upgrading the container keeps the same certificate and your browser's exception stays valid. The certificate is replaced only when it stops covering a name that is wanted (for example after the public URL's host changes, on the next restart), and it is renewed in place, while Bayplan runs, 30 days before it expires. The certificate's SHA-256 fingerprint is written to the log at start, so you can compare it with what your browser shows. Bayplan never sends HSTS with its own self-signed certificate, because a browser that remembered it could not click through a regenerated one; it sends HSTS only with a certificate you supply, or when the public URL is `https` and a proxy in front ends the TLS.

The image's health check follows the scheme by itself. Bayplan sends `Strict-Transport-Security` (one year) only when the browser reached it over HTTPS, either directly or because the public URL is `https://`. It is never sent over plain HTTP, where browsers ignore it. Once a browser has seen it for a host, that host is pinned to HTTPS for a year, so only use an `https://` public URL when HTTPS is meant to stay.

### Public URL

The public URL is the address people use to reach Bayplan. It can be set in the setup wizard, in **Settings > General**, or with `BAYPLAN_PUBLIC_URL`. Bayplan uses it to decide which browser origin may change anything: every state-changing request must come from that origin, and the session cookie is marked `Secure` (and named `__Host-bayplan_session`) when it is `https://`.

- When `BAYPLAN_PUBLIC_URL` is set it **locks** the value: the field in Settings becomes read-only and the API answers `409 public_url_locked`. Remove the variable to edit it in Settings again; the value stored earlier is used.
- **Changing the public URL to an address you are not browsing from can lock you out.** From then on Bayplan refuses sign-ins and changes made from the old address, because their origin no longer matches. Settings asks you to confirm when the new origin differs from the one you are using. If it goes wrong, reach Bayplan at the new address, or start it with `BAYPLAN_PUBLIC_URL` set to the address you use.

### Behind a reverse proxy

Set `BAYPLAN_PUBLIC_URL` to the address people type, and list the proxy in `BAYPLAN_TRUSTED_PROXIES` so client addresses in the audit log and the rate limits come from `X-Forwarded-For` rather than the proxy itself. The live event feed is a Server-Sent Events stream. Bayplan sends `X-Accel-Buffering: no` and a heartbeat every 15 seconds, so nginx needs no special location. Check these on any proxy:

- an idle or read timeout **shorter than 15 seconds** cuts the stream;
- compression of `text/event-stream` holds events back (Bayplan itself never compresses it);
- the proxy must pass `Origin` and `Host` through unchanged.

This build ships no proxy-specific configuration; treat the notes above as the contract.

## Data and backups

- Mount `/data` as a **named volume or a bind mount**. Data written to an unmounted `/data` is lost when the container is removed. Bayplan warns in the log and to admins in the app when `/data` is not a mount point or sits on tmpfs. It cannot tell an anonymous volume from a named one, so it gives **no warning for an anonymous volume**. An anonymous volume is easy to lose: a replacement container gets a new, empty one, and `docker run --rm` or `docker rm -v` deletes it.
- **Back up `/data/bayplan.key` together with the database** (`/data/bayplan.db`). The key encrypts stored secrets and keys the audit log's integrity chain. Lose it and those secrets cannot be read and `bayplan audit verify` cannot check the log. Bayplan shows a reminder until an admin acknowledges it. A backup that holds both the key and the database holds everything, so protect it like a password store. If you supply `BAYPLAN_ENCRYPTION_KEY` yourself, no key file is written and you keep that value safe.
- Before it applies a database migration to an existing database, Bayplan writes a snapshot to `/data/backups` and keeps the newest three. If the volume has less than twice the database's size free, it refuses to start rather than fill the volume; `BAYPLAN_SKIP_PRE_MIGRATE_BACKUP=true` skips the snapshot.
- **Downgrades are refused.** A database written by a newer schema stops an older binary at startup, naming both versions and the newest snapshot.
- Upgrade by replacing the container on the same volume:

  ```sh
  docker pull ghcr.io/ianglennon/bayplan:0.1
  docker rm -f bayplan
  docker run -d --name bayplan ... ghcr.io/ianglennon/bayplan:0.1
  ```

## Operations

Run these with `docker exec <container> bayplan ...`. Each drops to UID 1000 first, like the server.

| Command | What it does |
|---------|--------------|
| `bayplan healthcheck` | Exits 0 when the local server answers `/healthz`. This is the image's `HEALTHCHECK`. |
| `bayplan setup-token` | Prints the active setup token. Exits 1 once setup is complete. |
| `bayplan user unlock <name>` | Clears a locked account, for when the only admin is locked out. Recorded in the audit log as `system:cli`. Exits 1 if there is no such user. |
| `bayplan audit verify` | Recomputes the audit log's HMAC chain and names the first broken entry. Exits 1 on a broken chain. It cannot see entries deleted from the end of the log; see the threat model. |
| `bayplan version` | Prints the version and commit. |

The **audit log** (admins only) lists sign-ins, sign-outs, denials, setting changes and environment changes. It can be filtered and exported as CSV or JSON, newest entries first. In the CSV, a cell that starts with `=`, `+`, `-`, `@`, a tab or a carriage return gets a leading single quote so a spreadsheet does not run it as a formula; a value read back from the spreadsheet therefore differs from the stored one by that quote.

Changing your own password asks for the current one, allows five attempts at once and then one every 12 seconds, ends your other sessions, and is audited.

The source code, version and commit are linked from **Settings > About**, as the AGPL requires, together with the third-party notices.

## More

- [SECURITY.md](SECURITY.md): reporting a vulnerability, privately.
- [CONTRIBUTING.md](CONTRIBUTING.md): building, testing and sending changes.
- [docs/threat-model.md](docs/threat-model.md): what Bayplan protects, what it does not, and the risks that remain.
- [docs/release-checklist.md](docs/release-checklist.md): how a release is cut.
