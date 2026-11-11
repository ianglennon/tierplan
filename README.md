<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="web/src/assets/brand/tierplan-wordmark-dark.svg">
    <img alt="TierPlan" src="web/src/assets/brand/tierplan-wordmark-light.svg" width="360">
  </picture>
</h1>

TierPlan is an open-source, web-based manager for Docker: many Docker Swarm clusters and standalone Docker hosts, from one place. It aims to be easier to use than Portainer, with nothing held back behind a paid tier.

Licence: [AGPL-3.0-or-later](LICENSE). Anyone who runs a modified version as a network service must offer its users the source.

**Status: pre-release.** No version has been published yet; the first release will be v0.1.0, and breaking changes are allowed between 0.x minors. This build lets you install TierPlan, sign in, and look at the Docker host it runs on: a dashboard, containers, images, volumes and networks (read-only), a live event feed, an audit log and settings, users and teams with Viewer, Operator and Admin roles per environment, and encrypted backups you can restore. On a Docker Swarm it also manages services (scale, edit, force update, roll back, create and remove), nodes (drain, promote, label and remove), secrets and configs, shows where tasks run, and merges a service's logs and live stats across its tasks. It also manages **compose stacks**: write a stack in the editor, review what will change, and deploy it to a swarm or a standalone host, with every image pinned to a digest and nothing left behind when you remove it ([docs/stacks.md](docs/stacks.md)). It also shows where each environment's network address space has gone, warns before a swarm pool or overlay runs out, finds orphaned networks and picks right-sized subnets ([docs/networks.md](docs/networks.md)).

## Before you install

> **Mounting `/var/run/docker.sock` gives TierPlan, and every admin who signs in to it, root-equivalent control of that host.** Anyone who can use the Docker API can start a privileged container and read or change anything on the machine. Only mount the socket on a host you intend TierPlan to manage, protect the admin accounts accordingly, and see [docs/threat-model.md](docs/threat-model.md) for what this means in practice.

TierPlan sends no telemetry and makes no outbound calls you did not configure. The web interface loads nothing from other origins (no CDN, no analytics, bundled fonts), and its Content-Security-Policy keeps it that way.

## Install

```sh
docker run -d --name tierplan --restart unless-stopped \
  -p 7480:7480 \
  -v tierplan-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /proc/1/net/route:/host/route:ro \
  ghcr.io/ianglennon/tierplan:0.1
```

The last `-v` is optional: it lets the subnet picker check TierPlan's own host's routes against a subnet you choose; without it that check is skipped and says so.

The same thing as a Compose file:

```yaml
services:
  tierplan:
    image: ghcr.io/ianglennon/tierplan:0.1
    container_name: tierplan
    restart: unless-stopped
    ports:
      - "7480:7480"
    volumes:
      - tierplan-data:/data
      - /var/run/docker.sock:/var/run/docker.sock
      - /proc/1/net/route:/host/route:ro  # optional, see above

volumes:
  tierplan-data:
```

Image tags:

- `:edge` is built from the `main` branch and is not a supported release.
- From a release on, `X.Y.Z` is that exact version and `X.Y` follows the patch releases of one minor line. Use `:X.Y` (for example `:0.1`).
- There is **no `:latest` tag** while TierPlan is 0.x. Every 0.x minor may break the one before it, so a command you copy must name the line it was written for. The in-app "no Docker socket" screens show the same command, with the tag that matches the running build.

The image is published for `linux/amd64` and `linux/arm64`. It is based on distroless `static` (no shell, no package manager).

### How the container runs

The container starts as root, and only for a moment. TierPlan fixes the ownership of `/data` (a new bind mount is root-owned), joins the group that owns the Docker socket, and then drops itself to UID and GID 1000 before it opens the database or serves a request. The same drop happens for every `docker exec <container> tierplan ...` command. If the socket's group cannot be detected, set `TIERPLAN_DOCKER_GID` to the group ID to join instead.

## First run

1. Open `https://HOST:7480`. Your browser warns that the certificate is not trusted: TierPlan made it for this install. Compare the SHA-256 fingerprint the browser shows with the one in `docker logs tierplan` (the line that starts "TierPlan serves HTTPS with its own self-signed certificate"), then continue. A new instance shows the setup wizard.
2. The wizard asks for the **setup token**. Every start of an unclaimed instance prints a new one to the container log, and writes it to `/data/setup-token`:

   ```sh
   docker logs tierplan
   docker exec tierplan tierplan setup-token
   ```

3. Choose the first admin's user name and a password (12 characters or more; common passwords and passwords that contain the user name are refused), then an instance name and the public URL. The wizard fills in the `https://` address you are using; if `TIERPLAN_PUBLIC_URL` is set, that value is shown read-only.
4. After setup, TierPlan asks whether to manage the Docker host it runs on. Nothing is managed until an admin says yes. Choosing yes is the step that gives TierPlan control of the host.
5. Later, sign in at the same address with the account you created. After five failed attempts an account locks for 30 minutes (see [Operations](#operations) to clear a lock).

Once you are in, [docs/access.md](docs/access.md) explains how to invite teammates, put them in teams and give them the Viewer, Operator or Admin role on each environment.

Anyone who can read the container log or `/data` can read the setup token until setup is finished; the token file is deleted when setup completes. Claim a new instance promptly, and do not expose port 7480 to a network you do not trust before you have.

## Requirements

- Docker Engine **24 to 29**, rootful, on Linux (`amd64` or `arm64`). TierPlan checks the API version and tells you when an engine is older than Docker 24 (API 1.43).
- **Not supported:** rootless Docker, Docker Desktop and Podman. TierPlan detects each of them and says why it cannot manage the host.
- `DOCKER_HOST` is honoured for `unix://` paths only. `tcp://` and `ssh://` addresses are refused at startup.
- On a **Docker Swarm**, run TierPlan on a manager node and as exactly one replica (the database is SQLite, a single writer). A service example, not exercised in Phase 1:

  ```sh
  docker service create --name tierplan --replicas 1 \
    --constraint 'node.role==manager' \
    --constraint 'node.hostname==MANAGER-HOSTNAME' \
    --publish 7480:7480 \
    --mount type=volume,source=tierplan-data,target=/data \
    --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock \
    ghcr.io/ianglennon/tierplan:0.1
  ```

  A named volume lives on one node, so pin the service to that node as well as to the managers.

## Configuration

Every setting is an environment variable, and most are also a command-line flag (`tierplan serve --help` lists them). A flag that is set wins over the environment, and the environment wins over the default. Secrets are not available as flags, so they stay out of the process list; they accept a `_FILE` variant that reads the value from a file, for Docker secrets. Setting both `X` and `X_FILE` is an error. An invalid value stops startup and the message names the variable.

| Variable | Default | `_FILE` | Description |
|----------|---------|---------|-------------|
| `TIERPLAN_LISTEN` | `:7480` | no | Address to listen on, as `host:port` or `:port`. |
| `TIERPLAN_DATA_DIR` | `/data` | no | Directory for the database, the encryption key and a copy of the public TLS certificate. |
| `TIERPLAN_PUBLIC_URL` | unset | no | Externally visible base URL, for example `https://tierplan.example.com`. Must start with `https://`: TierPlan refuses to start with an `http://` one. Locks the stored value (see [Public URL](#public-url)). |
| `TIERPLAN_AGENT_URL` | unset | no | Address agents dial when it differs from the public URL, for example an internal host name or a LAN address such as `https://tierplan.internal:7480`. Must start with `https://`. Locks the stored value; the API answers `409 agent_url_locked`. Unset means agents dial the public URL. |
| `TIERPLAN_ENCRYPTION_KEY` | generated | yes | Key for secrets at rest and the audit chain: 32 random bytes as standard base64. When unset, a key is generated in `/data/tierplan.key`. |
| `TIERPLAN_TRUSTED_PROXIES` | unset | no | Comma-separated IPs or CIDRs of reverse proxies whose forwarded headers are believed. |
| `TIERPLAN_LOGIN_MAX_FAILURES` | `5` | no | Failed sign-ins before an account locks. |
| `TIERPLAN_LOGIN_LOCKOUT` | `30m` | no | How long a locked account stays locked. |
| `TIERPLAN_SESSION_IDLE_TIMEOUT` | `12h` | no | Idle time after which a session ends. |
| `TIERPLAN_SESSION_REMEMBER_LIFETIME` | `720h` | no | Lifetime of a "keep me signed in" session. |
| `TIERPLAN_INVITE_TTL` | `72h` | no | How long a one-time invite or password reset link works, from `1h` to `720h`. |
| `TIERPLAN_AUDIT_RETENTION_DAYS` | `365` | no | Days to keep audit entries; `0` keeps them forever. |
| `TIERPLAN_SKIP_PRE_MIGRATE_BACKUP` | `false` | no | Do not snapshot the database before a migration. |
| `TIERPLAN_RESTORE_RESTART` | `exec` | no | How TierPlan restarts after a restore from the web UI. `exec` replaces the process with a fresh TierPlan, so the container keeps running. `exit` stops with status 0 after the swap and relies on the container's restart policy (`unless-stopped` or `always`) to start it again. |
| `TIERPLAN_BACKUP_MAX_UPLOAD` | `1GiB` | no | The largest backup file the web UI accepts for a restore, from `1MiB` to `64GiB`, in bytes or with a `KiB`, `MiB` or `GiB` suffix. A reverse proxy in front of TierPlan needs an upload limit at least this large. |
| `TIERPLAN_BACKUP_DIR` | `<data>/backups` | no | The folder scheduled backups are written into. Mount a volume or a host folder here to keep backups off the data volume. Only scheduled `tierplan-backup-*.tierplan` files in it are ever listed, served or deleted. `tierplan backup` with no `--out` also writes here, as `tierplan-manual-<date>-<time>.tierplan`, which retention leaves alone and the Backup page does not list. |
| `TIERPLAN_BACKUP_PASSPHRASE_FILE` | unset | n/a | Used by the command-line `tierplan backup` and `tierplan restore`, not by the server: the file holding the backup passphrase. Without it the commands read the first line of standard input. The passphrase is never a flag. |
| `TIERPLAN_TLS_CERT_FILE` | unset | no | Path to a TLS certificate in PEM form. Needs `TIERPLAN_TLS_KEY_FILE`. |
| `TIERPLAN_TLS_KEY_FILE` | unset | no | Path to the TLS private key in PEM form. Needs `TIERPLAN_TLS_CERT_FILE`. |
| `TIERPLAN_ACME_DIRECTORY_URL` | unset | no | Testing only and unsupported: an `https://` ACME directory URL to use instead of Let's Encrypt production (Pebble and other test CAs). Unset means Let's Encrypt production. It cannot be changed from the web UI or the API. |
| `TIERPLAN_ACME_CA_FILE` | unset | no | Testing only and unsupported: PEM file of the CA that signs the ACME directory's HTTPS certificate. Needs `TIERPLAN_ACME_DIRECTORY_URL`. Trusted by the ACME client only, never added to the system roots. |
| `TIERPLAN_ACME` | unset | no | `on` or `off`: turns Let's Encrypt on or off. Locks the Issue and Stop buttons in Settings > HTTPS and names this variable there; the stored value is kept underneath and used again if you remove the variable. `on` needs `TIERPLAN_ACME_ACCEPT_TOS=true`. |
| `TIERPLAN_ACME_ACCEPT_TOS` | unset | no | `true` accepts the certificate authority's terms of service. Required with `TIERPLAN_ACME=on`; any other value stops startup. Locks the terms checkbox in Settings > HTTPS. |
| `TIERPLAN_ACME_EMAIL` | unset | no | Contact address for the Let's Encrypt account, a bare address such as `admin@example.com` (no display name). Locks the email field in Settings > HTTPS; the stored value is kept underneath and used again if you remove the variable. |
| `TIERPLAN_ACME_CHALLENGE` | unset | no | `http-01` or `dns-01`, in any case. Locks the challenge choice in Settings > HTTPS; the stored value is kept and used again if you remove the variable. |
| `TIERPLAN_DNS_PROVIDER` | unset | no | `cloudflare`, the DNS provider for `dns-01`. Needs `TIERPLAN_CLOUDFLARE_API_TOKEN` or `TIERPLAN_CLOUDFLARE_API_TOKEN_FILE`, and the token needs it. Locks the provider choice; the stored value is kept and used again if you remove the variable. |
| `TIERPLAN_CLOUDFLARE_API_TOKEN` | unset | yes | Cloudflare API token for `dns-01` (the Zone DNS Edit permission, scoped to the zone). Prefer `TIERPLAN_CLOUDFLARE_API_TOKEN_FILE`, which reads it from a file such as a Docker secret: a plain variable stays visible in `docker inspect`. Setting both is an error. Locks the token field and is never logged. |
| `TIERPLAN_DNS_RESOLVERS` | unset | no | Comma-separated DNS resolvers (`host` or `host:port`, at most 5) the `dns-01` propagation check asks, in your order. Unset means the zone's own nameservers. A value that lists no resolver stops startup. Locks the resolver list; the stored value is kept and used again if you remove the variable. |
| `TIERPLAN_ACME_PROFILE` | unset | no | The CA's certificate profile name, such as `shortlived` (1 to 64 characters of `a-z`, `0-9`, `.`, `_`, `-`). Locks the profile choice; the stored value is kept and used again if you remove the variable. |
| `TIERPLAN_HSTS` | unset | no | `on` or `off`: turns the Strict-Transport-Security header on or off. `on` needs a trusted certificate and is refused at start together with `TIERPLAN_ACME=off`, because stopping Let's Encrypt under HSTS would lock browsers out. Locks the HSTS switch; the stored value is kept and used again if you remove the variable. |
| `TIERPLAN_TLS_SELF_SIGNED` | `false` | no | Deprecated and without effect: TierPlan always serves HTTPS, with its own certificate unless you supply one. Setting it only logs a warning; remove it. |
| `TIERPLAN_DOCKER_GID` | detected | no | Group ID of the Docker socket, overriding detection. |
| `TIERPLAN_HOST_ROUTE_FILE` | `/host/route` | no | The host's `/proc/1/net/route` bind-mounted read-only into TierPlan's own container (`-v /proc/1/net/route:/host/route:ro`), so the subnet picker can warn about a subnet that collides with a LAN or VPN route of the host TierPlan runs on. An absolute path. Optional: without the mount the check says it cannot read this host's routes and nothing else changes. See [docs/agent.md](docs/agent.md#host-routes). |
| `TIERPLAN_AGENT_HEARTBEAT_INTERVAL` | `10s` | no | How often each agent sends a heartbeat; the value is sent to the agent when it connects. |
| `TIERPLAN_AGENT_OFFLINE_AFTER` | `30s` | no | Silence after which an agent connection is closed and the agent counts as offline. At least twice the heartbeat interval. |
| `TIERPLAN_AGENT_RECONNECT_GRACE` | `60s` | no | After TierPlan starts, how long an environment shows "Reconnecting" instead of offline while its agents come back. |
| `TIERPLAN_FAILING_SERVICE_GRACE` | `2m` | no | How long after a swarm service was updated it is not counted as failing, so a rolling update does not show as a failure. |
| `TIERPLAN_STACK_CONVERGE_TIMEOUT` | `10m` | no | How long a swarm stack deploy waits for each service to settle, from `1m` to `2h`. A service whose tasks are not running by then reads Timed out and the deploy stops at it; Docker may still finish it. |
| `TIERPLAN_AGENT_CONNECT_BURST` | `200` | no | How many connection attempts one client address may make at once on the agent endpoint, then one more every 100 ms. Sized so a whole swarm behind one NAT can reconnect together. At most 16 handshakes from one address run at once, and an agent must answer the challenge within 3 s. |
| `TIERPLAN_LOG_LEVEL` | `info` | no | `debug`, `info`, `warn` or `error`. Logs are JSON on standard error. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | no | Docker daemon address; `unix://` only. Pointing it at a different daemon still shows the "TierPlan (this instance)" badge on the one TierPlan runs in. |

Only `TIERPLAN_ENCRYPTION_KEY` and `TIERPLAN_CLOUDFLARE_API_TOKEN` accept `_FILE` (`TIERPLAN_ENCRYPTION_KEY_FILE`, `TIERPLAN_CLOUDFLARE_API_TOKEN_FILE`). TierPlan logs every setting at startup with secrets shown as `[redacted]`.

### TLS

TierPlan always serves HTTPS on its port (`TIERPLAN_LISTEN`, default 7480), TLS 1.2 or newer, with HTTP/2 for browsers. There is no plain-HTTP mode and no setting to turn HTTPS off.

- **Its own certificate (the default):** on the first start TierPlan makes an ECDSA P-256 key and a self-signed certificate valid for 10 years. The key is sealed in the database with the other secrets, so it is in every backup, and a restore onto a new host serves the same certificate and keeps the pin your agents hold. The certificate covers `localhost`, the loopback addresses, the machine's host name and the hosts of the public URL and the agent URL. A host name that is a container ID (12 or 64 hex characters, which Docker assigns and changes on every recreation) is left out, so recreating or upgrading the container keeps the same certificate and your browser's exception stays valid. TierPlan signs the certificate again, with the same key, only when it stops covering a name it needs (for example after the public URL's host changes, on the next restart). It is never renewed on a timer, and the key never changes by itself. The public certificate is also written to `/data/tls/server.crt`, which the image's health check reads. The certificate's SHA-256 fingerprint is logged at start, so you can compare it with what your browser shows.
- **Your certificate:** mount the files and set `TIERPLAN_TLS_CERT_FILE` and `TIERPLAN_TLS_KEY_FILE`. Both or neither. This takes precedence over the certificate above. When you replace the files, TierPlan picks up the new pair within a minute, with no restart; a replacement that cannot be loaded is logged and the working certificate stays in use.
- **`TIERPLAN_TLS_SELF_SIGNED`** is deprecated and has no effect. Remove it.

Plain HTTP on the same port serves one thing: `/.well-known/acme-challenge/`, which answers 404 until Let's Encrypt support lands. Every other plain request is not answered: TierPlan closes the connection without sending a byte. That includes health probes and requests from the machine itself, so point a reverse proxy at `https://`, not `http://` ([docs/reverse-proxy.md](docs/reverse-proxy.md)). Agents connect over `wss://` only.

The image's health check probes `/healthz` over `https` and checks the answer against TierPlan's own certificate. TierPlan sends `Strict-Transport-Security` (one year) only on a response it sends over TLS with a certificate you supplied. It never sends it with its own self-signed certificate, because a browser that remembered it could not click through a regenerated one. A proxy that ends TLS for browsers can send its own HSTS. Once a browser has seen it for a host, that host is pinned to HTTPS for a year, so only use it when HTTPS is meant to stay.

If you are upgrading an install that served plain HTTP, see [docs/upgrade.md](docs/upgrade.md#upgrading-from-a-plain-http-install).

### Public URL

The public URL is the address people use to reach TierPlan. It must start with `https://`: the setup wizard and **Settings > General** refuse an `http://` address, and TierPlan will not start with one stored or set (see [docs/upgrade.md](docs/upgrade.md#upgrading-from-a-plain-http-install)). It can be set in the setup wizard, in **Settings > General**, or with `TIERPLAN_PUBLIC_URL`. TierPlan uses it to decide which browser origin may change anything: every state-changing request must come from that origin. The session cookie is always `Secure`, and is named `__Host-tierplan_session` (or `__Secure-tierplan_session` under a subpath).

- When `TIERPLAN_PUBLIC_URL` is set it **locks** the value: the field in Settings becomes read-only and the API answers `409 public_url_locked`. Remove the variable to edit it in Settings again; the value stored earlier is used.
- **Changing the public URL to an address you are not browsing from can lock you out.** From then on TierPlan refuses sign-ins and changes made from the old address, because their origin no longer matches. Settings asks you to confirm when the new origin differs from the one you are using. If it goes wrong, reach TierPlan at the new address, or start it with `TIERPLAN_PUBLIC_URL` set to the address you use.

### Behind a reverse proxy

Point the proxy at `https://tierplan:7480` (HTTPS upstream; TierPlan does not answer plain HTTP). Set `TIERPLAN_PUBLIC_URL` to the address people type, and list the proxy in `TIERPLAN_TRUSTED_PROXIES` so client addresses in the audit log and the rate limits come from `X-Forwarded-For` rather than the proxy itself. The live event feed is a Server-Sent Events stream. TierPlan sends `X-Accel-Buffering: no` and a heartbeat every 15 seconds, so nginx needs no special location. Check these on any proxy:

- an idle or read timeout **shorter than 15 seconds** cuts the stream;
- compression of `text/event-stream` holds events back (TierPlan itself never compresses it);
- the proxy must pass `Origin` and `Host` through unchanged.

[docs/reverse-proxy.md](docs/reverse-proxy.md) has working Caddy, nginx and Traefik configurations at the root of a host and at a subpath, says which forwarded headers TierPlan believes and which it ignores, and explains the proxy warnings that appear in the bell. `make proxy-smoke` tests all three.

## Add a remote host or swarm

TierPlan reaches a remote Docker host or swarm through a small agent that you install there with one command. The agent dials out to TierPlan, so the host opens no inbound port and exposes no Docker socket on the network. In **Environments**, choose **Add environment**, copy the swarm or standalone command it shows, and run it on a swarm manager or on the host. The page watches for the agent and opens the environment when it connects.

TierPlan needs to be reachable from the host over `wss://` with a certificate the agent can check: a certificate from a public CA, TierPlan's own self-signed certificate (the command carries its pin), or a proxy's private certificate (give the agent `TIERPLAN_AGENT_PIN`). Behind a reverse proxy the proxy must pass WebSocket upgrades on `/agent/connect` and be listed in `TIERPLAN_TRUSTED_PROXIES`; see [docs/reverse-proxy.md](docs/reverse-proxy.md).

[docs/agent.md](docs/agent.md) is the operator's guide: the swarm and standalone installs and what each flag does, rotating the join token, the read-only and self-update settings, private-CA proxies, what every connection problem means and how to fix it, cloned machines, revoking a node, and removing the agent again.

## Data and backups

- Mount `/data` as a **named volume or a bind mount**. Data written to an unmounted `/data` is lost when the container is removed. TierPlan warns in the log and to admins in the app when `/data` is not a mount point or sits on tmpfs. It cannot tell an anonymous volume from a named one, so it gives **no warning for an anonymous volume**. An anonymous volume is easy to lose: a replacement container gets a new, empty one, and `docker run --rm` or `docker rm -v` deletes it.
- **Back up TierPlan** from **Settings > Backup**, or with `docker exec -i <container> tierplan backup --out - > file.tierplan`. A backup is one encrypted file that holds users, teams, access, settings, environments and the audit log, and restores onto a new install through the setup wizard, **Settings > Backup** or `tierplan restore`. It can also run on a schedule. [docs/backup.md](docs/backup.md) covers what it holds, the three ways to restore, agents after a restore and rotating the master key.
- **Back up `/data/tierplan.key` together with the database** (`/data/tierplan.db`). The key encrypts stored secrets and keys the audit log's integrity chain. Lose it and those secrets cannot be read and `tierplan audit verify` cannot check the log. TierPlan shows a reminder until an admin acknowledges it. A backup that holds both the key and the database holds everything, so protect it like a password store. If you supply `TIERPLAN_ENCRYPTION_KEY` yourself, no key file is written and you keep that value safe.
- Before it applies a database migration to an existing database, TierPlan writes a snapshot to `/data/backups` and keeps the newest three. If the volume has less than twice the database's size free, it refuses to start rather than fill the volume; `TIERPLAN_SKIP_PRE_MIGRATE_BACKUP=true` skips the snapshot.
- **Downgrades are refused.** A database written by a newer schema stops an older binary at startup, naming both versions and the newest snapshot.
- **Upgrading** means replacing the container on the same volume. [docs/upgrade.md](docs/upgrade.md) covers `docker run` and compose, what happens on start, how to check the audit chain afterwards and how to roll back. TierPlan never checks for new versions itself.

## Operations

Run these with `docker exec <container> tierplan ...`. Each drops to UID 1000 first, like the server.

| Command | What it does |
|---------|--------------|
| `tierplan healthcheck` | Exits 0 when the local server answers `/healthz`. This is the image's `HEALTHCHECK`. |
| `tierplan setup-token` | Prints the active setup token. Exits 1 once setup is complete. |
| `tierplan user unlock <name>` | Clears a locked account, for when the only admin is locked out. Recorded in the audit log as `system:cli`. Exits 1 if there is no such user. |
| `tierplan user reset-password <name>` | Ends every session of a user and prints a one-time link that lets them choose a new password, for when the only admin has forgotten theirs. The link is built from the stored public URL and base path; with none stored it is printed as `/invite#token=…` to open on your TierPlan address. Recorded in the audit log as `system:cli`. Exits 1 if there is no such user or the user is disabled. |
| `tierplan backup [--out <file>\|-]` | Writes an encrypted `.tierplan` backup while TierPlan runs, by default as `tierplan-manual-<date>-<time>.tierplan` in `TIERPLAN_BACKUP_DIR` or `<data>/backups`, never replacing a file. The passphrase comes from the file named by `TIERPLAN_BACKUP_PASSPHRASE_FILE` or from standard input, never from a flag. Recorded in the audit log as `system:cli`. See [docs/backup.md](docs/backup.md). |
| `tierplan restore [--yes] <file>` | Restores a backup into the data directory. Run it as a one-off container on the same volume with TierPlan stopped: it refuses while TierPlan holds the data directory. Without `--yes` it only shows what the file holds. Saves the current database as `backups/prerestore-<time>.db` first. |
| `tierplan key rotate` | Replaces the master key, re-encrypts every stored secret and re-signs the audit log. Run it as a one-off container with TierPlan stopped. The old key is kept as `tierplan.key.old`; with `TIERPLAN_ENCRYPTION_KEY` the new key is written to `tierplan.key.new` for you to set. |
| `tierplan permissions` | Prints the role-to-permission matrix, generated from the permission registry. It is the source of [docs/permissions.md](docs/permissions.md). |
| `tierplan audit verify` | Recomputes the audit log's HMAC chain and names the first broken entry. Exits 1 on a broken chain. It cannot see entries deleted from the end of the log; see the threat model. |
| `tierplan version` | Prints the version and commit. |

The **audit log** (admins only) lists sign-ins, sign-outs, denials, setting changes and environment changes. It can be filtered and exported as CSV or JSON, newest entries first. In the CSV, a cell that starts with `=`, `+`, `-`, `@`, a tab or a carriage return gets a leading single quote so a spreadsheet does not run it as a formula; a value read back from the spreadsheet therefore differs from the stored one by that quote.

Changing your own password asks for the current one, allows five attempts at once and then one every 12 seconds, ends your other sessions, and is audited.

The source code, version and commit are linked from **Settings > About**, as the AGPL requires, together with the third-party notices.

## More

- [SECURITY.md](SECURITY.md): reporting a vulnerability, privately.
- [CONTRIBUTING.md](CONTRIBUTING.md): building, testing and sending changes.
- [docs/reverse-proxy.md](docs/reverse-proxy.md): running TierPlan behind Caddy, nginx or Traefik, at the root or a subpath.
- [docs/containers.md](docs/containers.md): containers and bulk actions, logs, browser shells, networks, volumes and events, and what each one records.
- [docs/stacks.md](docs/stacks.md): compose stacks: the editor, stack files and variables, the review, image pinning, deploying to a swarm or a standalone host, stopping, starting and removing, the labels TierPlan writes, and what is still open.
- [docs/networks.md](docs/networks.md): the Address space page, pool and overlay warnings, orphaned and unused networks, the subnet picker, small stack subnets, and the pool guidance.
- [docs/janitor.md](docs/janitor.md): the janitor: what it removes on its own and what it never touches, the schedule and policy levels, keeping things with `tierplan.keep=true`, previews and Clean now, run history, crash evidence, and the swarm task history limit.
- [docs/services.md](docs/services.md): swarm services, nodes, secrets and configs, service logs and stats, restart loops, and why TierPlan cannot change a service with host access unless the host allows it.
- [docs/access.md](docs/access.md): instance admins, roles, teams, invite and reset links, sessions.
- [docs/permissions.md](docs/permissions.md): what each role may do, generated from the code.
- [docs/backup.md](docs/backup.md): backups, the three ways to restore, agents after a restore and rotating the master key.
- [docs/upgrade.md](docs/upgrade.md): moving to a newer version and going back.
- [docs/agent.md](docs/agent.md): installing, securing, troubleshooting and removing the agent on a remote host or swarm.
- [docs/agent-updates.md](docs/agent-updates.md): updating and removing agents by hand.
- [docs/threat-model.md](docs/threat-model.md): what TierPlan protects, what it does not, and the risks that remain.
- [docs/release-checklist.md](docs/release-checklist.md): how a release is cut.
