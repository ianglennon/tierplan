# Running TierPlan behind a reverse proxy

TierPlan serves HTTPS only: it listens on port 7480 and answers with its own certificate unless you give it one. **A reverse proxy in front of it must talk HTTPS to it** (`https://tierplan:7480`), and most people use a proxy to end TLS for the browser with a proper host name. This page says what TierPlan needs from that proxy, and gives working configurations for Caddy, nginx and Traefik, each at the root of a host and at a subpath.

The configurations on this page are the files in [`docs/proxy/`](proxy/). `make proxy-smoke` runs the built image behind all three, at both places, and checks what is listed under [Checking a setup](#checking-a-setup).

## Proxy or TierPlan's own HTTPS?

You do not always need a proxy. TierPlan can get a free certificate that browsers trust from Let's Encrypt and serve it itself, over HTTP-01 or, with Cloudflare DNS, DNS-01. [HTTPS and Let's Encrypt](https.md) says how.

Use a proxy when:

- one already ends TLS for other sites on this host;
- TierPlan must live under a subpath of a shared host name;
- you need a wildcard certificate, or a certificate from another CA;
- your DNS is not on Cloudflare and port 80 cannot reach TierPlan.

Otherwise TierPlan's own Let's Encrypt support is simpler: there is no second program to run and no second certificate to keep. If you do use a proxy, the rest of this page applies, and the proxy still talks HTTPS to TierPlan.

## The short version

1. Point the proxy at `https://tierplan:7480` (HTTPS upstream), not at `http://`. TierPlan does not answer plain HTTP. See [HTTPS to TierPlan](#https-to-tierplan).
2. Give TierPlan its own host name, such as `tierplan.example.com`. A subpath works (see [Root or subpath](#root-or-subpath)), but a host name of its own is the setup to prefer.
3. Set `TIERPLAN_PUBLIC_URL` to the address people type, for example `https://tierplan.example.com`.
4. Set `TIERPLAN_TRUSTED_PROXIES` to the address or network of the proxy, for example `172.18.0.0/16`.
5. In the proxy: pass `Host` through, send `X-Forwarded-For` and `X-Forwarded-Proto`, pass the WebSocket upgrade, do not buffer, and allow long-lived connections.

If something is wrong, TierPlan tells admins in the bell at the top of the page. See [When the bell shows a proxy warning](#when-the-bell-shows-a-proxy-warning).

## HTTPS to TierPlan

TierPlan serves HTTPS and nothing else, on its own port, with its own certificate (a self-signed one it makes at first start, or the one you give it). The proxy ends TLS for the browser with the certificate for your host name and opens a second TLS connection to TierPlan. The public URL is the proxy's `https://` host; TierPlan keeps serving its own certificate on its port, which is also what agents and anyone connecting directly use.

**Trust TierPlan's certificate in the proxy (recommended).** TierPlan keeps a copy of its certificate at `/data/tls/server.crt` in its data volume. Share that volume with the proxy read-only, or copy the file out, and tell the proxy to trust it and to check it for a host name the certificate carries. The simplest name is `tierplan`: give the TierPlan container that host name (`hostname: tierplan`), or use the host of its public URL. The three files below do this: `tls_trust_pool` and `tls_server_name` in Caddy, `proxy_ssl_trusted_certificate`, `proxy_ssl_verify on` and `proxy_ssl_name` in nginx, and a `serversTransport` with `rootCAs` and `serverName` in Traefik.

**Skipping verification is your choice, not the default.** If you would rather not share the file, each proxy can be told to accept any certificate from TierPlan: `tls_insecure_skip_verify` inside Caddy's `transport http` block, `insecureSkipVerify: true` in Traefik's `serversTransport`, or `proxy_ssl_verify off;` in nginx. The traffic is still encrypted, but the proxy can no longer tell TierPlan from something else answering at that address on the network. The files in this repository never do it, and a check in the repository fails if they do.

**Existing proxied installs must be reconfigured to HTTPS upstream.** Before this change TierPlan spoke plain HTTP behind a proxy, and the proxy was pointed at an `http://` address on port 7480. TierPlan no longer serves plain requests, so a proxy that still dials `http://` gets no answer. Switch the proxy to `https://tierplan:7480` as described above when you upgrade TierPlan. TierPlan also no longer counts a plain request marked `X-Forwarded-Proto: https` as secure: the connection to TierPlan itself must be TLS. `TIERPLAN_TRUSTED_PROXIES` still decides whose `X-Forwarded-For` and `X-Forwarded-Proto` TierPlan believes, over that TLS connection.

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
| `X-Forwarded-Proto` | The scheme the client used, for the links and cookies TierPlan builds. It never makes a plain connection count as secure: the connection from the proxy to TierPlan must itself be TLS. With several values, the last one counts. |

TierPlan **ignores** `X-Forwarded-Host`, `X-Forwarded-Prefix` and the standard `Forwarded` header, from every peer. It also ignores `X-Forwarded-For` and `X-Forwarded-Proto` from any peer that is not trusted. Traefik adds `X-Forwarded-Prefix` when it strips a prefix; that is harmless.

Keep the list short. Everything in it can choose the client address of every request. For a proxy in a Docker network, use the network's range (`docker network inspect <name>` shows it as `Subnet`) or the proxy container's address. Do not list a whole private range that other machines can reach.

## Root or subpath

At the root of a host, the public URL has no path: `https://tierplan.example.com`. At a subpath it does: `https://example.com/tierplan`. The proxy removes the prefix before the request reaches TierPlan, so TierPlan sees `/api/version`, not `/tierplan/api/version`. (TierPlan also accepts a request that still has the prefix, so either style of proxy works.) `/healthz` and `/readyz` stay at the root of the container for its health check.

The session cookie's name depends on how TierPlan is reached:

| Public URL | Cookie | Path |
|------------|--------|------|
| `https://tierplan.example.com` | `__Host-tierplan_session` | `/` |
| `https://example.com/tierplan` | `__Secure-tierplan_session` | `/tierplan` |

A cookie with the `__Host-` prefix must have the path `/`, so a subpath uses `__Secure-` instead. Changing the public URL's path changes the cookie, which signs everyone out once.

**Prefer a host name of its own.** A cookie is scoped by host, not by origin: another web application on the same host name can set or replace cookies for the `/tierplan` path, and its pages run on the same origin as TierPlan's. On a host name of its own nothing else can.

The configurations below have a site for each: a dedicated host name, `tierplan.example.com`, and a subpath on a shared host, `example.com/tierplan`. Keep the one you want.

## Agents

Agents dial the public URL with `/agent/connect` added, over `wss://`, so the proxy must pass WebSocket upgrades on that path, and it reaches TierPlan over TLS like every other request. An agent that is refused with a message about the transport did not arrive over TLS: check that the proxy dials `https://tierplan:7480`.

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

Caddy sets `X-Forwarded-For` and `X-Forwarded-Proto` itself, passes WebSocket upgrades and flushes event streams as they arrive, and gets a certificate for a site address without a scheme. It dials TierPlan over HTTPS and checks TierPlan's certificate against the copy named in `tls_trust_pool`. `header_up Host {hostport}` passes on the host the browser sent, port included.

```
# Caddy in front of TierPlan. See docs/reverse-proxy.md.
#
# TierPlan serves HTTPS only, so Caddy talks HTTPS to it and checks TierPlan's
# certificate: tls_trust_pool names the copy of TierPlan's own certificate
# (/data/tls/server.crt in TierPlan's data volume, shared read-only with Caddy or
# copied out), and tls_server_name is a host name that certificate carries (give
# TierPlan the host name tierplan, or use the host of its public URL).
#
# Caddy gets and renews a public certificate for any site address that has no
# scheme, sets X-Forwarded-For and X-Forwarded-Proto by itself, passes WebSocket
# upgrades through and flushes text/event-stream responses at once. Caddy replaces
# Host when it dials an HTTPS upstream, so header_up Host passes on the one the
# browser sent, port included: TierPlan compares it with its public URL.

# A dedicated host name (recommended). Set TIERPLAN_PUBLIC_URL=https://tierplan.example.com.
tierplan.example.com {
	reverse_proxy https://tierplan:7480 {
		header_up Host {hostport}
		transport http {
			tls_trust_pool file /tierplan-data/tls/server.crt
			tls_server_name tierplan
		}
	}
}

# A subpath on a host you share. Set TIERPLAN_PUBLIC_URL=https://example.com/tierplan.
# handle_path removes /tierplan before the request reaches TierPlan.
example.com {
	handle_path /tierplan/* {
		reverse_proxy https://tierplan:7480 {
			header_up Host {hostport}
			transport http {
				tls_trust_pool file /tierplan-data/tls/server.crt
				tls_server_name tierplan
			}
		}
	}
	redir /tierplan /tierplan/ permanent
}
```

## nginx

This is a complete `nginx.conf`. It ends TLS on port 443 with your certificate (put it at the two `ssl_certificate` paths) and dials TierPlan over HTTPS, checking TierPlan's certificate (`proxy_ssl_verify on`). `$scheme` is `https`, so `X-Forwarded-Proto` says so by itself. The `Upgrade` and `Connection` lines are what keep WebSockets working: without them nginx drops the upgrade, the agent endpoint answers `426`, and the bell shows "WebSocket connections are failing".

```nginx
# nginx in front of TierPlan. See docs/reverse-proxy.md. This is a complete
# nginx.conf. It ends TLS for the browser on port 443 and talks HTTPS to TierPlan,
# which serves nothing else.
#
# Put your own certificate and key at the two ssl_certificate paths below.
# proxy_ssl_trusted_certificate is the copy of TierPlan's own certificate
# (/data/tls/server.crt in TierPlan's data volume, shared read-only with nginx or
# copied out); proxy_ssl_verify makes nginx check it, and proxy_ssl_name is a host
# name that certificate carries (give TierPlan the host name tierplan, or use the
# host of its public URL).

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

    # Check TierPlan's certificate. These apply to every location in this file.
    proxy_ssl_verify on;
    proxy_ssl_trusted_certificate /tierplan-data/tls/server.crt;
    proxy_ssl_server_name on;
    proxy_ssl_name tierplan;

    # A dedicated host name (recommended). Set TIERPLAN_PUBLIC_URL=https://tierplan.example.com.
    server {
        listen 443 ssl;
        server_name tierplan.example.com;
        ssl_certificate /etc/nginx/tls/cert.pem;
        ssl_certificate_key /etc/nginx/tls/key.pem;

        location / {
            set $tierplan https://tierplan:7480;
            proxy_pass $tierplan;
        }
    }

    # A subpath on a host you share. Set TIERPLAN_PUBLIC_URL=https://example.com/tierplan.
    # The rewrite removes /tierplan/ before the request reaches TierPlan. (With a
    # fixed host name, "proxy_pass https://tierplan:7480/;" does the same.)
    server {
        listen 443 ssl;
        server_name example.com;
        ssl_certificate /etc/nginx/tls/cert.pem;
        ssl_certificate_key /etc/nginx/tls/key.pem;

        location = /tierplan {
            return 308 /tierplan/;
        }

        location /tierplan/ {
            # "break" ends the rewrite directives, so set comes first.
            set $tierplan https://tierplan:7480;
            rewrite ^/tierplan/(.*)$ /$1 break;
            proxy_pass $tierplan;
        }
    }
}
```

## Traefik

This is Traefik's dynamic configuration for the file provider. Start Traefik with an entry point called `websecure` and this file:

```sh
traefik --entrypoints.websecure.address=:443 --providers.file.filename=/etc/traefik/tierplan.yml
```

The file ends TLS for the browser with the certificate at the two paths under `tls` (or use a certificate resolver instead), and the `serversTransport` checks TierPlan's certificate.

```yaml
# Traefik in front of TierPlan. See docs/reverse-proxy.md. This is Traefik's
# dynamic configuration for the file provider:
#
#   traefik --entrypoints.websecure.address=:443 \
#           --providers.file.filename=/etc/traefik/tierplan.yml
#
# It uses an entry point called "websecure" that ends TLS for the browser, and talks
# HTTPS to TierPlan, which serves nothing else. Put your own certificate and key at
# the two paths under "tls", or use a certificate resolver instead. The
# serversTransport checks TierPlan's certificate: rootCAs is the copy of TierPlan's
# own certificate (/data/tls/server.crt in TierPlan's data volume, shared read-only
# with Traefik or copied out), and serverName is a host name that certificate
# carries (give TierPlan the host name tierplan, or use the host of its public URL).
# Traefik passes WebSocket upgrades through by itself, flushes text/event-stream
# responses at once, and sets X-Forwarded-For, X-Forwarded-Proto and Host. It also
# adds X-Forwarded-Prefix when it strips a prefix; TierPlan ignores that header, so
# the public URL stays the only source of the base path.

tls:
  certificates:
    - certFile: /etc/traefik/tls/cert.pem
      keyFile: /etc/traefik/tls/key.pem

http:
  routers:
    # A dedicated host name (recommended). Set TIERPLAN_PUBLIC_URL=https://tierplan.example.com.
    tierplan-root:
      rule: "Host(`tierplan.example.com`)"
      entryPoints: [websecure]
      tls: {}
      service: tierplan

    # A subpath on a host you share. Set TIERPLAN_PUBLIC_URL=https://example.com/tierplan.
    tierplan-sub:
      rule: "Host(`example.com`) && (Path(`/tierplan`) || PathPrefix(`/tierplan/`))"
      entryPoints: [websecure]
      tls: {}
      middlewares: [tierplan-strip]
      service: tierplan

  middlewares:
    tierplan-strip:
      stripPrefix:
        prefixes: ["/tierplan"]

  serversTransports:
    tierplan:
      rootCAs:
        - /tierplan-data/tls/server.crt
      serverName: tierplan

  services:
    tierplan:
      loadBalancer:
        serversTransport: tierplan
        servers:
          - url: "https://tierplan:7480"
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

`make proxy-smoke` builds the image (`make e2e-image`), runs it behind Caddy, nginx and Traefik with the files above at the root of a host and at `/tierplan`, with TLS on both sides (each proxy ends TLS with a generated certificate and checks TierPlan's own), and prints a `PASS` or `FAIL` line for each check:

- the index carries the base path;
- `/api/version` answers;
- a `wss://` upgrade of `/agent/connect` with the subprotocol `tierplan.agent.v1` gets `101 Switching Protocols`, reaching TierPlan over TLS;
- the signed-in event stream sends its first frame at once, and without a session it answers `401` at once;
- the shell's upgrade request (`shell_ws_reaches_tierplan`) and a log stream request (`log_stream_prompt`) reach TierPlan, which answers them itself;
- TierPlan shows no proxy warning.

The last two checks ask for an environment that does not exist, so they need no container. They show that the proxy forwards the shell's upgrade request, with the session and the origin, to TierPlan, which answers with its own `environment_not_found` problem and not a `400`, `426` or `502` from the proxy, and that the log stream request is answered within 3 seconds and not held back. They do not open a real shell or stream real log lines; the end-to-end suite does that in Chromium behind Caddy at `/tierplan`. Behind nginx and Traefik these two request-level checks are all there is.

It then breaks nginx three ways, and checks that TierPlan notices each: without the `Upgrade` lines (the agent endpoint answers `426` and the bell shows the WebSocket warning), without the `Host` line (the public URL warning), and without `TIERPLAN_TRUSTED_PROXIES` (the untrusted address warning). It needs Docker, Go, `curl` and `jq`, and removes everything it creates. `PROXY_SMOKE_ONLY=caddy-root,nginx-subpath make proxy-smoke` runs only the scenarios named (`KIND-SITE`, such as `traefik-root`, or `nginx-root-no_host_header` for a deliberate break).
