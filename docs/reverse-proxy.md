# Running TierPlan behind a reverse proxy

TierPlan listens on port 7480 and speaks plain HTTP unless you give it a certificate. Most people put a proxy in front of it that ends TLS and gives it a proper host name. This page says what TierPlan needs from that proxy, and gives working configurations for Caddy, nginx and Traefik, each at the root of a host and at a subpath.

The configurations on this page are the files in [`docs/proxy/`](proxy/). `make proxy-smoke` runs the built image behind all three, at both places, and checks what is listed under [Checking a setup](#checking-a-setup).

## The short version

1. Give TierPlan its own host name, such as `tierplan.example.com`. A subpath works (see [Root or subpath](#root-or-subpath)), but a host name of its own is the setup to prefer.
2. Set `TIERPLAN_PUBLIC_URL` to the address people type, for example `https://tierplan.example.com`.
3. Set `TIERPLAN_TRUSTED_PROXIES` to the address or network of the proxy, for example `172.18.0.0/16`.
4. In the proxy: pass `Host` through, send `X-Forwarded-For` and `X-Forwarded-Proto`, pass the WebSocket upgrade, do not buffer, and allow long-lived connections.

If something is wrong, TierPlan tells admins in the bell at the top of the page. See [When the bell shows a proxy warning](#when-the-bell-shows-a-proxy-warning).

## The public URL decides everything

TierPlan takes its host name, its base path and the origin it accepts changes from, from the public URL and from nothing else. Whatever a proxy or a client puts in a header cannot change them. This is deliberate: a header that could widen the set of accepted origins would let any page on the internet change things as an admin.

- **Origin and CSRF.** Every change a browser makes must come from the public URL's origin. A request from another origin is refused.
- **Base path.** The path of the public URL (`/tierplan` in `https://example.com/tierplan`) is the base path. TierPlan serves the app, the API, the event streams and the agent endpoint under it.
- **Cookies.** The session cookie is scoped to that host and path. See [Root or subpath](#root-or-subpath).

Set the public URL with `TIERPLAN_PUBLIC_URL`, in the setup wizard or in **Settings > General**.

## What TierPlan believes from a proxy

TierPlan believes forwarded headers only from the addresses in `TIERPLAN_TRUSTED_PROXIES`, a comma-separated list of IP addresses and CIDR ranges. From a peer on that list it reads two headers:

| Header | What TierPlan does with it |
|--------|---------------------------|
| `X-Forwarded-For` | The client address, for the audit log and the rate limits. TierPlan takes the rightmost address that is not itself a trusted proxy, so a client cannot choose its address by sending the header first. |
| `X-Forwarded-Proto` | The scheme the client used. `https` from a trusted proxy lets agents connect over `wss://` and tells TierPlan the request was secure. With several values, the last one counts. |

TierPlan **ignores** `X-Forwarded-Host`, `X-Forwarded-Prefix` and the standard `Forwarded` header, from every peer. It also ignores `X-Forwarded-For` and `X-Forwarded-Proto` from any peer that is not trusted. Traefik adds `X-Forwarded-Prefix` when it strips a prefix; that is harmless.

Keep the list short. Everything in it can choose the client address of every request. For a proxy in a Docker network, use the network's range (`docker network inspect <name>` shows it as `Subnet`) or the proxy container's address. Do not list a whole private range that other machines can reach.

## Root or subpath

At the root of a host, the public URL has no path: `https://tierplan.example.com`. At a subpath it does: `https://example.com/tierplan`. The proxy removes the prefix before the request reaches TierPlan, so TierPlan sees `/api/version`, not `/tierplan/api/version`. (TierPlan also accepts a request that still has the prefix, so either style of proxy works.) `/healthz` and `/readyz` stay at the root of the container for its health check.

The session cookie's name depends on how TierPlan is reached:

| Public URL | Cookie | Path |
|------------|--------|------|
| `https://tierplan.example.com` | `__Host-tierplan_session` | `/` |
| `https://example.com/tierplan` | `__Secure-tierplan_session` | `/tierplan` |
| `http://…` | `tierplan_session` | the base path, or `/` |

A cookie with the `__Host-` prefix must have the path `/`, so a subpath uses `__Secure-` instead. Changing the public URL's scheme or path changes the cookie, which signs everyone out once.

**Prefer a host name of its own.** A cookie is scoped by host, not by origin: another web application on the same host name can set or replace cookies for the `/tierplan` path, and its pages run on the same origin as TierPlan's. On a host name of its own nothing else can.

The configurations below have a site for each: a dedicated host name, `tierplan.example.com`, and a subpath on a shared host, `example.com/tierplan`. Keep the one you want.

## Agents

Agents dial the public URL with `/agent/connect` added, over `wss://`, so the proxy must pass WebSocket upgrades on that path. An agent that is refused with a message about the transport is reaching TierPlan over plain HTTP without a trusted proxy saying `https`: check `TIERPLAN_TRUSTED_PROXIES` and the `X-Forwarded-Proto` line.

When the hosts that run agents reach TierPlan at a different address than people do (for example an internal name or a LAN address), set the agent URL with `TIERPLAN_AGENT_URL`, such as `https://tierplan.internal:7480`. The install commands then carry that address, and TierPlan accepts agents that dial it. You can change it later in Settings, General. Agents connected over `wss://` are sent the new address straight away and dial it from their next connection. TierPlan then accepts agents only at the new host and port, with no fallback period. Agents that were offline or on plain `ws://` at that moment are refused until you reinstall them with the new address or change it back. See [Change TierPlan's agent address](agent.md#change-tierplans-agent-address).

## Server-Sent Events, WebSockets and timeouts

TierPlan's live feeds are Server-Sent Events streams, and the agent tunnel and the terminals are WebSockets. Both are long-lived, so a proxy must:

- **pass the upgrade.** A WebSocket request carries `Upgrade: websocket` and `Connection: Upgrade`. nginx drops both unless told to pass them; Caddy and Traefik pass them by themselves.
- **not buffer the streams.** A buffering proxy holds events back until a buffer fills, so the page looks frozen. TierPlan sends `X-Accel-Buffering: no`, which nginx honours, and the nginx file below also sets `proxy_buffering off`. Do not compress `text/event-stream`.
- **allow idle connections.** TierPlan sends something every 10 to 20 seconds on every connection: agent heartbeats every 10 seconds, a comment line on each event stream and each log stream every 15 seconds, and a ping on each browser shell every 20 seconds. That fits inside the 60 second idle timeout that nginx starts with. A longer timeout keeps connections up through a pause, so the files below set one hour for nginx. Traefik's entry point closes idle connections after 180 seconds by default, and Caddy sets no limit. Never set an idle or read timeout under 15 seconds.

Phase 4 adds two kinds of long-lived connection to this list. Both pass through the same proxy settings and need nothing extra:

- **The browser shell** is a WebSocket at `/api/env/<environment>/shell-sessions/<session>/ws` (under your base path, if you use one). It is opened from the page after a normal `POST`, and TierPlan accepts the upgrade only from your public URL's origin (or the request's own host), so the proxy must pass the `Origin` and `Host` headers unchanged. A shell can sit open for hours, so a proxy idle or read timeout must not be shorter than the 20 second ping. TierPlan closes a shell that nobody uses after 30 minutes by default.
- **Log streams** are Server-Sent Events at `/api/env/<environment>/containers/<container>/logs/stream`, and a log download (`.../logs/download`) is one long response that is written as it is read. Neither may be buffered or compressed, or the log appears in bursts, or only when the download ends. A service's log (Phase 6) is the same at `/api/env/<environment>/services/<service>/logs/stream` and `.../logs/download`, with the same rules.
- **Stats streams** (Phase 6) are Server-Sent Events at `/api/env/<environment>/containers/<container>/stats/stream`, one sample about every two seconds. Like a log stream, one may not be buffered or compressed, or the charts update in bursts. A stats stream takes one of the browser's connections for as long as the Stats tab is open and visible, and the page closes it after a minute in a hidden tab. The Stats tab of a service, a list with the Live stats switch on and the placement page with it on each open one more such stream (`.../services/<service>/stats/stream`, `.../services/stats/stream`, `.../containers/stats/stream` and `.../nodes/stats/stream`) that must not be buffered either.

Several log windows open at once can reach a browser limit. On plain HTTP/1.1 a browser opens at most six connections to one origin, and each live log, each event stream and each shell takes one of them, so a seventh does not start until another closes. HTTP/2 shares many streams over one connection and removes the limit, so serve TierPlan over HTTPS from a proxy that speaks HTTP/2 if you keep many logs open. Caddy and Traefik do this over HTTPS by default, and nginx does when HTTP/2 is switched on for the listener. The files below do not set this up for you.

The proxy smoke test (`make proxy-smoke`) checks that each of the three setups below passes the shell's upgrade request and a log stream request through to TierPlan. See [Checking a setup](#checking-a-setup).

## Caddy

Caddy sets `X-Forwarded-For`, `X-Forwarded-Proto` and `Host` itself, passes WebSocket upgrades and flushes event streams as they arrive, and gets a certificate for a site address without a scheme. The file reads one environment variable, `TIERPLAN_SCHEME`, which is empty normally; the smoke test sets it to `http://` to run the same file without a certificate.

```
# Caddy in front of TierPlan. See docs/reverse-proxy.md.
#
# Caddy gets and renews a certificate for any site address that has no scheme,
# sets X-Forwarded-For, X-Forwarded-Proto and Host by itself, passes WebSocket
# upgrades through and flushes text/event-stream responses at once, so there is
# nothing else to add. TIERPLAN_SCHEME is empty in normal use. The smoke test
# (scripts/proxy-smoke.sh) sets it to "http://" to run this same file without a
# certificate.

# A dedicated host name (recommended). Set TIERPLAN_PUBLIC_URL=https://tierplan.example.com.
{$TIERPLAN_SCHEME}tierplan.example.com {
	reverse_proxy tierplan:7480
}

# A subpath on a host you share. Set TIERPLAN_PUBLIC_URL=https://example.com/tierplan.
# handle_path removes /tierplan before the request reaches TierPlan.
{$TIERPLAN_SCHEME}example.com {
	handle_path /tierplan/* {
		reverse_proxy tierplan:7480
	}
	redir /tierplan /tierplan/ permanent
}
```

## nginx

This is a complete `nginx.conf`. It listens on port 80; to end TLS in nginx, change each `listen 80;` to `listen 443 ssl;` and add `ssl_certificate` and `ssl_certificate_key`. `$scheme` then becomes `https` by itself. The `Upgrade` and `Connection` lines are what keep WebSockets working: without them nginx drops the upgrade, the agent endpoint answers `426`, and the bell shows "WebSocket connections are failing".

```nginx
# nginx in front of TierPlan. See docs/reverse-proxy.md. This is a complete
# nginx.conf. It listens on port 80; to end TLS in nginx, change each
# "listen 80;" to "listen 443 ssl;" and add ssl_certificate and
# ssl_certificate_key. X-Forwarded-Proto then says https by itself.

events {}

http {
    # Close the connection to TierPlan unless the client asked for an upgrade.
    map $http_upgrade $connection_upgrade {
        default upgrade;
        ''      close;
    }

    # The settings below apply to every location in this file.
    proxy_http_version 1.1;
    # The host the client asked for, port included. TierPlan compares it with the
    # public URL.
    proxy_set_header Host              $http_host;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    # WebSocket upgrade: the agents' tunnel and the terminal need both lines.
    proxy_set_header Upgrade           $http_upgrade;
    proxy_set_header Connection        $connection_upgrade;
    # Server-Sent Events: do not hold events back.
    proxy_buffering off;
    # nginx closes an idle connection after 60 seconds by default. TierPlan sends
    # something every 10 to 15 seconds, so the default works, but a long timeout
    # keeps the tunnel and the live feeds up through a pause.
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;

    # nginx looks up a host name in proxy_pass once, at start, and keeps the
    # address. Replacing the TierPlan container usually gives it a new address, so
    # the name is looked up again every 10 seconds instead, which needs a resolver
    # and the name in a variable. 127.0.0.11 is Docker's built-in DNS; if nginx
    # does not run in a Docker network, use your own resolver.
    resolver 127.0.0.11 valid=10s;

    # A dedicated host name (recommended). Set TIERPLAN_PUBLIC_URL=https://tierplan.example.com.
    server {
        listen 80;
        server_name tierplan.example.com;

        location / {
            set $tierplan http://tierplan:7480;
            proxy_pass $tierplan;
        }
    }

    # A subpath on a host you share. Set TIERPLAN_PUBLIC_URL=https://example.com/tierplan.
    # The rewrite removes /tierplan/ before the request reaches TierPlan. (With a
    # fixed host name, "proxy_pass http://tierplan:7480/;" does the same.)
    server {
        listen 80;
        server_name example.com;

        location = /tierplan {
            return 308 /tierplan/;
        }

        location /tierplan/ {
            # "break" ends the rewrite directives, so set comes first.
            set $tierplan http://tierplan:7480;
            rewrite ^/tierplan/(.*)$ /$1 break;
            proxy_pass $tierplan;
        }
    }
}
```

## Traefik

This is Traefik's dynamic configuration for the file provider. Start Traefik with an entry point called `web` and this file:

```sh
traefik --entrypoints.web.address=:80 --providers.file.filename=/etc/traefik/tierplan.yml
```

To end TLS in Traefik, add an entry point for port 443 with a certificate resolver and name it in each router's `entryPoints`.

```yaml
# Traefik in front of TierPlan. See docs/reverse-proxy.md. This is Traefik's
# dynamic configuration for the file provider:
#
#   traefik --entrypoints.web.address=:80 \
#           --providers.file.filename=/etc/traefik/tierplan.yml
#
# It uses an entry point called "web". To end TLS in Traefik, add an entry point
# for port 443 with a certificate resolver and name it in each router's
# entryPoints. Traefik passes WebSocket upgrades through by itself, flushes
# text/event-stream responses at once, and sets X-Forwarded-For, X-Forwarded-Proto
# and Host. It also adds X-Forwarded-Prefix when it strips a prefix; TierPlan
# ignores that header, so the public URL stays the only source of the base path.

http:
  routers:
    # A dedicated host name (recommended). Set TIERPLAN_PUBLIC_URL=https://tierplan.example.com.
    tierplan-root:
      rule: "Host(`tierplan.example.com`)"
      entryPoints: [web]
      service: tierplan

    # A subpath on a host you share. Set TIERPLAN_PUBLIC_URL=https://example.com/tierplan.
    tierplan-sub:
      rule: "Host(`example.com`) && (Path(`/tierplan`) || PathPrefix(`/tierplan/`))"
      entryPoints: [web]
      middlewares: [tierplan-strip]
      service: tierplan

  middlewares:
    tierplan-strip:
      stripPrefix:
        prefixes: ["/tierplan"]

  services:
    tierplan:
      loadBalancer:
        servers:
          - url: "http://tierplan:7480"
```

## When the bell shows a proxy warning

TierPlan watches the requests that reach it and tells admins (anyone with the `settings.read` permission) in the bell. A warning goes away on its own an hour after the last request that caused it. Each one links here.

| Warning | What it means | What to do |
|---------|---------------|------------|
| **Forwarded headers from an untrusted address** | A peer that is not in `TIERPLAN_TRUSTED_PROXIES` sent `X-Forwarded-…` headers. TierPlan ignored them. | If the address in the warning is your proxy, add it to `TIERPLAN_TRUSTED_PROXIES`. If it is not, something is sending forged headers; they did no harm. |
| **Requests don't match the public URL** | Requests arrive for a host, or over a scheme, that is not the public URL's. | Make the proxy pass the original `Host` (nginx: `proxy_set_header Host $http_host;`) and send `X-Forwarded-Proto`, and check that `TIERPLAN_PUBLIC_URL` is the address people type. Reaching TierPlan by another address, such as its IP and port, shows this warning too. |
| **WebSocket connections are failing** | A request to `/agent/connect` arrived without the WebSocket upgrade headers. | Pass `Upgrade` and `Connection` (nginx: the two `proxy_set_header` lines and the `map`). |

Other symptoms:

- **Signing in works, then you are sent back to the sign-in page, or changes are refused with a message about the origin.** The public URL is not the address in the browser's address bar. Compare them, scheme and path included.
- **The page loads but the Live indicator says Reconnecting, or events arrive in bursts.** The proxy is buffering or timing out the event stream. Turn buffering off for it and raise the idle timeout.
- **An agent keeps being refused.** `docker logs` of the agent names the reason, and the audit log has an `agent.enrol_refused` entry for each refused attempt.

## Checking a setup

`make proxy-smoke` builds the image (`make e2e-image`), runs it behind Caddy, nginx and Traefik with the files above at the root of a host and at `/tierplan`, and prints a `PASS` or `FAIL` line for each check:

- the index carries the base path;
- `/api/version` answers;
- a WebSocket upgrade of `/agent/connect` with the subprotocol `tierplan.agent.v1` gets `101 Switching Protocols`;
- the signed-in event stream sends its first frame at once, and without a session it answers `401` at once;
- the shell's upgrade request (`shell_ws_reaches_tierplan`) and a log stream request (`log_stream_prompt`) reach TierPlan, which answers them itself;
- TierPlan shows no proxy warning.

The last two checks ask for an environment that does not exist, so they need no container. They show that the proxy forwards the shell's upgrade request, with the session and the origin, to TierPlan, which answers with its own `environment_not_found` problem and not a `400`, `426` or `502` from the proxy, and that the log stream request is answered within 3 seconds and not held back. They do not open a real shell or stream real log lines; the end-to-end suite does that in Chromium behind Caddy at `/tierplan`. Behind nginx and Traefik these two request-level checks are all there is.

It then breaks nginx three ways, and checks that TierPlan notices each: without the `Upgrade` lines (the agent endpoint answers `426` and the bell shows the WebSocket warning), without the `Host` line (the public URL warning), and without `TIERPLAN_TRUSTED_PROXIES` (the untrusted address warning). It needs Docker, `curl` and `jq`, and removes everything it creates.
