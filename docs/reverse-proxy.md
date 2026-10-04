# Running Bayplan behind a reverse proxy

Bayplan listens on port 7480 and speaks plain HTTP unless you give it a certificate. Most people put a proxy in front of it that ends TLS and gives it a proper host name. This page says what Bayplan needs from that proxy, and gives working configurations for Caddy, nginx and Traefik, each at the root of a host and at a subpath.

The configurations on this page are the files in [`docs/proxy/`](proxy/). `make proxy-smoke` runs the built image behind all three, at both places, and checks what is listed under [Checking a setup](#checking-a-setup).

## The short version

1. Give Bayplan its own host name, such as `bayplan.example.com`. A subpath works (see [Root or subpath](#root-or-subpath)), but a host name of its own is the setup to prefer.
2. Set `BAYPLAN_PUBLIC_URL` to the address people type, for example `https://bayplan.example.com`.
3. Set `BAYPLAN_TRUSTED_PROXIES` to the address or network of the proxy, for example `172.18.0.0/16`.
4. In the proxy: pass `Host` through, send `X-Forwarded-For` and `X-Forwarded-Proto`, pass the WebSocket upgrade, do not buffer, and allow long-lived connections.

If something is wrong, Bayplan tells admins in the bell at the top of the page. See [When the bell shows a proxy warning](#when-the-bell-shows-a-proxy-warning).

## The public URL decides everything

Bayplan takes its host name, its base path and the origin it accepts changes from, from the public URL and from nothing else. Whatever a proxy or a client puts in a header cannot change them. This is deliberate: a header that could widen the set of accepted origins would let any page on the internet change things as an admin.

- **Origin and CSRF.** Every change a browser makes must come from the public URL's origin. A request from another origin is refused.
- **Base path.** The path of the public URL (`/bayplan` in `https://example.com/bayplan`) is the base path. Bayplan serves the app, the API, the event streams and the agent endpoint under it.
- **Cookies.** The session cookie is scoped to that host and path. See [Root or subpath](#root-or-subpath).

Set the public URL with `BAYPLAN_PUBLIC_URL`, in the setup wizard or in **Settings > General**.

## What Bayplan believes from a proxy

Bayplan believes forwarded headers only from the addresses in `BAYPLAN_TRUSTED_PROXIES`, a comma-separated list of IP addresses and CIDR ranges. From a peer on that list it reads two headers:

| Header | What Bayplan does with it |
|--------|---------------------------|
| `X-Forwarded-For` | The client address, for the audit log and the rate limits. Bayplan takes the rightmost address that is not itself a trusted proxy, so a client cannot choose its address by sending the header first. |
| `X-Forwarded-Proto` | The scheme the client used. `https` from a trusted proxy lets agents connect over `wss://` and tells Bayplan the request was secure. With several values, the last one counts. |

Bayplan **ignores** `X-Forwarded-Host`, `X-Forwarded-Prefix` and the standard `Forwarded` header, from every peer. It also ignores `X-Forwarded-For` and `X-Forwarded-Proto` from any peer that is not trusted. Traefik adds `X-Forwarded-Prefix` when it strips a prefix; that is harmless.

Keep the list short. Everything in it can choose the client address of every request. For a proxy in a Docker network, use the network's range (`docker network inspect <name>` shows it as `Subnet`) or the proxy container's address. Do not list a whole private range that other machines can reach.

## Root or subpath

At the root of a host, the public URL has no path: `https://bayplan.example.com`. At a subpath it does: `https://example.com/bayplan`. The proxy removes the prefix before the request reaches Bayplan, so Bayplan sees `/api/version`, not `/bayplan/api/version`. (Bayplan also accepts a request that still has the prefix, so either style of proxy works.) `/healthz` and `/readyz` stay at the root of the container for its health check.

The session cookie's name depends on how Bayplan is reached:

| Public URL | Cookie | Path |
|------------|--------|------|
| `https://bayplan.example.com` | `__Host-bayplan_session` | `/` |
| `https://example.com/bayplan` | `__Secure-bayplan_session` | `/bayplan` |
| `http://…` | `bayplan_session` | the base path, or `/` |

A cookie with the `__Host-` prefix must have the path `/`, so a subpath uses `__Secure-` instead. Changing the public URL's scheme or path changes the cookie, which signs everyone out once.

**Prefer a host name of its own.** A cookie is scoped by host, not by origin: another web application on the same host name can set or replace cookies for the `/bayplan` path, and its pages run on the same origin as Bayplan's. On a host name of its own nothing else can.

The configurations below have a site for each: a dedicated host name, `bayplan.example.com`, and a subpath on a shared host, `example.com/bayplan`. Keep the one you want.

## Agents

Agents dial the public URL with `/agent/connect` added, over `wss://`, so the proxy must pass WebSocket upgrades on that path. An agent that is refused with a message about the transport is reaching Bayplan over plain HTTP without a trusted proxy saying `https`: check `BAYPLAN_TRUSTED_PROXIES` and the `X-Forwarded-Proto` line.

When the hosts that run agents reach Bayplan at a different address than people do (for example an internal name or a LAN address), set the agent URL with `BAYPLAN_AGENT_URL`, such as `https://bayplan.internal:7480`. The install commands then carry that address, and Bayplan accepts agents that dial it. You can change it later in Settings, General. Bayplan tells every connected agent the new address over its tunnel straight away, and each agent dials the new address first and keeps the old one as a fallback for 30 days. The settings page warns you how many agents are offline at that moment: they cannot be told, and Bayplan accepts agents on one address at a time, so an agent that was offline and still dials a different host than the new address cannot connect until it is reinstalled with the new address.

## Server-Sent Events, WebSockets and timeouts

Bayplan's live feeds are Server-Sent Events streams, and the agent tunnel and the terminals are WebSockets. Both are long-lived, so a proxy must:

- **pass the upgrade.** A WebSocket request carries `Upgrade: websocket` and `Connection: Upgrade`. nginx drops both unless told to pass them; Caddy and Traefik pass them by themselves.
- **not buffer the streams.** A buffering proxy holds events back until a buffer fills, so the page looks frozen. Bayplan sends `X-Accel-Buffering: no`, which nginx honours, and the nginx file below also sets `proxy_buffering off`. Do not compress `text/event-stream`.
- **allow idle connections.** Bayplan sends something every 10 to 15 seconds on every connection: agent heartbeats every 10 seconds, and a comment line on each event stream every 15 seconds. That fits inside the 60 second idle timeout that nginx starts with. A longer timeout keeps connections up through a pause, so the files below set one hour for nginx. Traefik's entry point closes idle connections after 180 seconds by default, and Caddy sets no limit. Never set an idle or read timeout under 15 seconds.

## Caddy

Caddy sets `X-Forwarded-For`, `X-Forwarded-Proto` and `Host` itself, passes WebSocket upgrades and flushes event streams as they arrive, and gets a certificate for a site address without a scheme. The file reads one environment variable, `BAYPLAN_SCHEME`, which is empty normally; the smoke test sets it to `http://` to run the same file without a certificate.

```
# Caddy in front of Bayplan. See docs/reverse-proxy.md.
#
# Caddy gets and renews a certificate for any site address that has no scheme,
# sets X-Forwarded-For, X-Forwarded-Proto and Host by itself, passes WebSocket
# upgrades through and flushes text/event-stream responses at once, so there is
# nothing else to add. BAYPLAN_SCHEME is empty in normal use. The smoke test
# (scripts/proxy-smoke.sh) sets it to "http://" to run this same file without a
# certificate.

# A dedicated host name (recommended). Set BAYPLAN_PUBLIC_URL=https://bayplan.example.com.
{$BAYPLAN_SCHEME}bayplan.example.com {
	reverse_proxy bayplan:7480
}

# A subpath on a host you share. Set BAYPLAN_PUBLIC_URL=https://example.com/bayplan.
# handle_path removes /bayplan before the request reaches Bayplan.
{$BAYPLAN_SCHEME}example.com {
	handle_path /bayplan/* {
		reverse_proxy bayplan:7480
	}
	redir /bayplan /bayplan/ permanent
}
```

## nginx

This is a complete `nginx.conf`. It listens on port 80; to end TLS in nginx, change each `listen 80;` to `listen 443 ssl;` and add `ssl_certificate` and `ssl_certificate_key`. `$scheme` then becomes `https` by itself. The `Upgrade` and `Connection` lines are what keep WebSockets working: without them nginx drops the upgrade, the agent endpoint answers `426`, and the bell shows "WebSocket connections are failing".

```nginx
# nginx in front of Bayplan. See docs/reverse-proxy.md. This is a complete
# nginx.conf. It listens on port 80; to end TLS in nginx, change each
# "listen 80;" to "listen 443 ssl;" and add ssl_certificate and
# ssl_certificate_key. X-Forwarded-Proto then says https by itself.

events {}

http {
    # Close the connection to Bayplan unless the client asked for an upgrade.
    map $http_upgrade $connection_upgrade {
        default upgrade;
        ''      close;
    }

    # The settings below apply to every location in this file.
    proxy_http_version 1.1;
    # The host the client asked for, port included. Bayplan compares it with the
    # public URL.
    proxy_set_header Host              $http_host;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    # WebSocket upgrade: the agents' tunnel and the terminal need both lines.
    proxy_set_header Upgrade           $http_upgrade;
    proxy_set_header Connection        $connection_upgrade;
    # Server-Sent Events: do not hold events back.
    proxy_buffering off;
    # nginx closes an idle connection after 60 seconds by default. Bayplan sends
    # something every 10 to 15 seconds, so the default works, but a long timeout
    # keeps the tunnel and the live feeds up through a pause.
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;

    # nginx looks up a host name in proxy_pass once, at start, and keeps the
    # address. Replacing the Bayplan container usually gives it a new address, so
    # the name is looked up again every 10 seconds instead, which needs a resolver
    # and the name in a variable. 127.0.0.11 is Docker's built-in DNS; if nginx
    # does not run in a Docker network, use your own resolver.
    resolver 127.0.0.11 valid=10s;

    # A dedicated host name (recommended). Set BAYPLAN_PUBLIC_URL=https://bayplan.example.com.
    server {
        listen 80;
        server_name bayplan.example.com;

        location / {
            set $bayplan http://bayplan:7480;
            proxy_pass $bayplan;
        }
    }

    # A subpath on a host you share. Set BAYPLAN_PUBLIC_URL=https://example.com/bayplan.
    # The rewrite removes /bayplan/ before the request reaches Bayplan. (With a
    # fixed host name, "proxy_pass http://bayplan:7480/;" does the same.)
    server {
        listen 80;
        server_name example.com;

        location = /bayplan {
            return 308 /bayplan/;
        }

        location /bayplan/ {
            # "break" ends the rewrite directives, so set comes first.
            set $bayplan http://bayplan:7480;
            rewrite ^/bayplan/(.*)$ /$1 break;
            proxy_pass $bayplan;
        }
    }
}
```

## Traefik

This is Traefik's dynamic configuration for the file provider. Start Traefik with an entry point called `web` and this file:

```sh
traefik --entrypoints.web.address=:80 --providers.file.filename=/etc/traefik/bayplan.yml
```

To end TLS in Traefik, add an entry point for port 443 with a certificate resolver and name it in each router's `entryPoints`.

```yaml
# Traefik in front of Bayplan. See docs/reverse-proxy.md. This is Traefik's
# dynamic configuration for the file provider:
#
#   traefik --entrypoints.web.address=:80 \
#           --providers.file.filename=/etc/traefik/bayplan.yml
#
# It uses an entry point called "web". To end TLS in Traefik, add an entry point
# for port 443 with a certificate resolver and name it in each router's
# entryPoints. Traefik passes WebSocket upgrades through by itself, flushes
# text/event-stream responses at once, and sets X-Forwarded-For, X-Forwarded-Proto
# and Host. It also adds X-Forwarded-Prefix when it strips a prefix; Bayplan
# ignores that header, so the public URL stays the only source of the base path.

http:
  routers:
    # A dedicated host name (recommended). Set BAYPLAN_PUBLIC_URL=https://bayplan.example.com.
    bayplan-root:
      rule: "Host(`bayplan.example.com`)"
      entryPoints: [web]
      service: bayplan

    # A subpath on a host you share. Set BAYPLAN_PUBLIC_URL=https://example.com/bayplan.
    bayplan-sub:
      rule: "Host(`example.com`) && (Path(`/bayplan`) || PathPrefix(`/bayplan/`))"
      entryPoints: [web]
      middlewares: [bayplan-strip]
      service: bayplan

  middlewares:
    bayplan-strip:
      stripPrefix:
        prefixes: ["/bayplan"]

  services:
    bayplan:
      loadBalancer:
        servers:
          - url: "http://bayplan:7480"
```

## When the bell shows a proxy warning

Bayplan watches the requests that reach it and tells admins (anyone with the `settings.read` permission) in the bell. A warning goes away on its own an hour after the last request that caused it. Each one links here.

| Warning | What it means | What to do |
|---------|---------------|------------|
| **Forwarded headers from an untrusted address** | A peer that is not in `BAYPLAN_TRUSTED_PROXIES` sent `X-Forwarded-…` headers. Bayplan ignored them. | If the address in the warning is your proxy, add it to `BAYPLAN_TRUSTED_PROXIES`. If it is not, something is sending forged headers; they did no harm. |
| **Requests don't match the public URL** | Requests arrive for a host, or over a scheme, that is not the public URL's. | Make the proxy pass the original `Host` (nginx: `proxy_set_header Host $http_host;`) and send `X-Forwarded-Proto`, and check that `BAYPLAN_PUBLIC_URL` is the address people type. Reaching Bayplan by another address, such as its IP and port, shows this warning too. |
| **WebSocket connections are failing** | A request to `/agent/connect` arrived without the WebSocket upgrade headers. | Pass `Upgrade` and `Connection` (nginx: the two `proxy_set_header` lines and the `map`). |

Other symptoms:

- **Signing in works, then you are sent back to the sign-in page, or changes are refused with a message about the origin.** The public URL is not the address in the browser's address bar. Compare them, scheme and path included.
- **The page loads but the Live indicator says Reconnecting, or events arrive in bursts.** The proxy is buffering or timing out the event stream. Turn buffering off for it and raise the idle timeout.
- **An agent keeps being refused.** `docker logs` of the agent names the reason, and the audit log has an `agent.enrol_refused` entry for each refused attempt.

## Checking a setup

`make proxy-smoke` builds the image (`make e2e-image`), runs it behind Caddy, nginx and Traefik with the files above at the root of a host and at `/bayplan`, and prints a `PASS` or `FAIL` line for each check:

- the index carries the base path;
- `/api/version` answers;
- a WebSocket upgrade of `/agent/connect` with the subprotocol `bayplan.agent.v1` gets `101 Switching Protocols`;
- the signed-in event stream sends its first frame at once, and without a session it answers `401` at once;
- Bayplan shows no proxy warning.

It then breaks nginx three ways, and checks that Bayplan notices each: without the `Upgrade` lines (the agent endpoint answers `426` and the bell shows the WebSocket warning), without the `Host` line (the public URL warning), and without `BAYPLAN_TRUSTED_PROXIES` (the untrusted address warning). It needs Docker, `curl` and `jq`, and removes everything it creates.
