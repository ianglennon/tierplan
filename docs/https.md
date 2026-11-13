<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# HTTPS and Let's Encrypt

TierPlan always serves HTTPS. This page says what it serves by default, how to choose another certificate (your own files, or a free one from Let's Encrypt), how to set that up with HTTP-01 or with DNS-01 through Cloudflare, how to configure all of it from environment variables, what the warnings in the bell mean, and what to do when an order fails.

If a reverse proxy already ends TLS for you, you may not need any of this. See [reverse-proxy.md](reverse-proxy.md).

## How TierPlan serves HTTPS by default

TierPlan listens on one port, 7480 unless you change `TIERPLAN_LISTEN`. Everything on it is TLS 1.2 or newer, with HTTP/2 for browsers. There is no plain-HTTP mode and no setting to turn HTTPS off.

Until you give it something better, TierPlan serves a certificate it made for this install:

- On the first start it makes an ECDSA P-256 key and a self-signed certificate that is valid for 10 years. It is never renewed on a timer, and the key never changes by itself.
- The key is sealed in the database with the other secrets, so it is in every backup. A restore onto a new host serves the same certificate and keeps the pin your agents hold.
- The public certificate is also written to `/data/tls/server.crt`.
- The certificate's SHA-256 fingerprint is logged at start. Compare it with what your browser shows before you accept it.

A browser warns about this certificate because no public authority vouches for it. That is expected. It is safe to continue once the fingerprints match, and the warning goes away when you use a certificate a browser trusts.

Agents do not need a trusted certificate. The install command you copy from Add environment carries the pin of the certificate the agent will see, and the agent connects only if the certificate matches. Every agent also keeps the self-signed pin it receives when it connects. That is why TierPlan can fall back to its own certificate without cutting agents off (see the renewal section below).

### Coming from a plain-HTTP v0.1.0 install

Your stored public URL is an `http://` address, which TierPlan will not start with, so you must set `TIERPLAN_PUBLIC_URL` to the `https://` address once and reinstall any agent that was installed with a `ws://` command. The steps are in [Upgrading from a plain-HTTP install](upgrade.md#upgrading-from-a-plain-http-install). If you supply your own certificate files, read [HSTS for certificate files](upgrade.md#hsts-for-certificate-files) too.

## Choosing a mode

| Mode | Use it when | Browsers |
|------|-------------|----------|
| Self-signed (the default) | You are trying TierPlan, or only agents and a few people use it and you can accept a warning. | Warn until each person accepts the certificate. |
| Certificate files | You already have a certificate for the host name, from your own CA or a public one. | Trust it if its authority is public. |
| Let's Encrypt | TierPlan has a public DNS name and you want a free, trusted certificate that renews by itself. | Trust it. |
| A reverse proxy instead | A proxy already ends TLS for this host name, or you need a wildcard, another CA, or a DNS provider other than Cloudflare. | The proxy's certificate decides. See [reverse-proxy.md](reverse-proxy.md). |

### Certificate files

Mount the files and set `TIERPLAN_TLS_CERT_FILE` and `TIERPLAN_TLS_KEY_FILE`. Set both or neither. When you replace the files, TierPlan picks up the new pair within 1 minute, with no restart. A replacement it cannot load is logged and the working pair stays in use. If the certificate chain needs intermediate certificates, put them after the certificate in the same file.

Certificate files win over Let's Encrypt. While they are set, Let's Encrypt is paused: TierPlan keeps its Let's Encrypt certificate, order and account, and makes no request to the CA. Remove both variables and restart to use Let's Encrypt again. TierPlan never renews certificate files. It tells you in the bell when they are close to expiring (see the bell section below).

### Let's Encrypt

Pick the validation method that fits your network:

- HTTP-01 when the internet can reach port 80 on TierPlan's host. See the next section.
- DNS-01 when your DNS is on Cloudflare. It needs no open port, so it works for an install the internet cannot reach. See the section after.

Either way, open **Settings > HTTPS**, register an account, accept the Subscriber Agreement, choose the method, and press **Issue certificate**. Nothing is ordered until you press it, and TierPlan switches to the new certificate as soon as it is ready, with no restart and nobody signed out. To do it all from environment variables instead, see [Configuring everything with environment variables](#configuring-everything-with-environment-variables).

## Let's Encrypt with HTTP-01

Let's Encrypt checks over plain HTTP on port 80 of the host name in your public URL. TierPlan must be reachable there:

```sh
docker run -d --name tierplan --restart unless-stopped \
  -p 7480:7480 -p 80:7480 \
  -v tierplan-data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/ianglennon/tierplan:0.1
```

`-p 80:7480` sends port 80 on the host to TierPlan's usual port. TierPlan answers on 7480 for both TLS and plain requests, and the plain listener answers exactly one thing: `/.well-known/acme-challenge/`, and only while an order is in progress. Every other plain request gets no answer at all. Check your firewall and router as well.

Before it orders anything, TierPlan checks the names and refuses to start an order it knows will fail. Nothing is sent to Let's Encrypt when it refuses. The refusals, by code:

| Code | What it means | What to do |
|------|---------------|------------|
| `names_ip` | The public URL uses an IP address. | Set the public URL in **Settings > General** to a DNS name that points to this host. |
| `agent_url_ip` | The agent URL uses an IP address. | Set the agent URL to a DNS name, or clear it to use the public URL. |
| `names_wildcard` | A name is a wildcard. | Use a plain host name. Wildcard certificates are not supported. |
| `names_not_public` | A name is not a public DNS name: it has no dot, or ends in a private suffix such as `.lan` or `.home.arpa`. | Use a name under a domain you own, such as `tierplan.example.com`. |
| `base_path` | The public URL has a path. HTTP-01 needs TierPlan at the root of its host. | Remove the path from the public URL, or give TierPlan its own host name. |
| `agent_dial_host` | A connected agent reaches TierPlan at a host that is not on the certificate. | Reinstall those agents with a new install command, or set `TIERPLAN_AGENT_URL` to a host the certificate covers. Only agents that are connected now are checked. |

The certificate covers the host of the public URL, plus the host of the agent URL when it differs. IP addresses and wildcards cannot be used. Connected agents must reach TierPlan at a host the certificate covers (see `agent_dial_host` above), so set `TIERPLAN_AGENT_URL` to a covered host if they use another.

## Let's Encrypt with DNS-01 through Cloudflare

With DNS-01, TierPlan adds a TXT record at `_acme-challenge.<your host>` in your Cloudflare zone, waits for it to appear, lets Let's Encrypt check it and deletes it. No port needs to be open, and the host name may point at a private address.

### The token

Create a Cloudflare API token on Cloudflare's API tokens page with exactly two permissions, Zone:DNS:Edit and Zone:Zone:Read, and limit it to the one zone your host name is in. TierPlan does not accept the Global API Key, and a token for all zones is more than it needs. Keep the token in a file and give it to TierPlan with `TIERPLAN_CLOUDFLARE_API_TOKEN_FILE`, or enter it in **Settings > HTTPS**, where it is kept encrypted and never shown again.

### Test token

Press **Test token** before you issue. It finds the zone, creates a TXT record at each name with a comment that starts with `tierplan:`, reads it back, notes which resolvers see it, and deletes it. It places no order and does not contact Let's Encrypt, so it uses none of its rate limits. It can take a minute while TierPlan waits for the record to appear.

### Delegation

If `_acme-challenge.<your host>` is a CNAME, TierPlan follows it and writes the record at the target. The target must be in a Cloudflare zone this token can edit. There is nothing to set. Use this when your own zone is somewhere else: point a CNAME for `_acme-challenge` at a name in a Cloudflare zone you control.

### Split-horizon DNS and resolvers

While it waits for the record to appear, TierPlan asks the zone's own nameservers. If this host cannot reach the zone's nameservers on port 53 directly, for example behind a firewall that allows only your own DNS server, list the resolvers to ask in `TIERPLAN_DNS_RESOLVERS`, or under **Advanced DNS** in **Settings > HTTPS**. Each entry is `host` or `host:port`, separated by commas, at most 5. They must answer for public records: a resolver that serves an internal view of the zone will not see the TXT record. Unset means the zone's own nameservers.

### Clean-up

TierPlan deletes only the records it created, by the record ID it stored when it made them, never by searching for a name. If a delete fails, the record is left and retried on a backoff, and the bell tells you (see the bell section below). The retries pause while Let's Encrypt is paused after a restore, or while certificate files are set. You can always delete a record yourself in the Cloudflare dashboard.

## Configuring everything with environment variables

Every HTTPS setting can come from the environment, so a compose file can bring up a working HTTPS install with nobody pressing a button. Lead with the token, and give it as a file:

| Variable | What it does |
|----------|--------------|
| `TIERPLAN_CLOUDFLARE_API_TOKEN_FILE` | The path of a file that holds the Cloudflare token, such as a Docker secret. Use this one. A plain `TIERPLAN_CLOUDFLARE_API_TOKEN` variable stays visible in `docker inspect`. Setting both is an error. |
| `TIERPLAN_ACME` | `on` or `off`. `on` orders and renews certificates by itself. |
| `TIERPLAN_ACME_ACCEPT_TOS` | `true` accepts the Subscriber Agreement. Required with `TIERPLAN_ACME=on`. |
| `TIERPLAN_ACME_EMAIL` | The contact address for the Let's Encrypt account, such as `admin@example.com`. Optional. |
| `TIERPLAN_ACME_CHALLENGE` | `http-01` or `dns-01`. |
| `TIERPLAN_DNS_PROVIDER` | `cloudflare`, for `dns-01`. |
| `TIERPLAN_DNS_RESOLVERS` | The resolvers the DNS-01 check asks, at most 5. |
| `TIERPLAN_ACME_PROFILE` | The CA's certificate profile name, such as `shortlived`. Unset means the CA's default. |
| `TIERPLAN_HSTS` | `on` or `off`. See the HSTS section below. |

Also relevant: `TIERPLAN_PUBLIC_URL` (the address the certificate must cover), `TIERPLAN_AGENT_URL`, `TIERPLAN_TLS_CERT_FILE` and `TIERPLAN_TLS_KEY_FILE`. The README lists every variable with its default.

How they behave:

- A variable overrides its setting and locks the field in **Settings > HTTPS**, which names the variable. The value stored earlier is kept underneath and used again if you remove the variable. Nothing from the environment is written to the database.
- `TIERPLAN_ACME=on` needs `TIERPLAN_ACME_ACCEPT_TOS=true`, and any other value for the terms stops startup.
- `TIERPLAN_ACME=off` is refused at start while Let's Encrypt is in use and HSTS is on, because it would serve the self-signed certificate to browsers that hold the policy. Turn HSTS off first.
- With `TIERPLAN_ACME=on` TierPlan orders the certificate by itself as soon as it is ready. A restart does not make it try sooner.

A whole setup for DNS-01, with the token from a Docker secret. The token file holds the token and nothing else.

```yaml
services:
  tierplan:
    image: ghcr.io/ianglennon/tierplan:0.1
    container_name: tierplan
    restart: unless-stopped
    ports:
      - "7480:7480"
      # HTTP-01 only: Let's Encrypt checks port 80. Not needed for DNS-01.
      # - "80:7480"
    volumes:
      - tierplan-data:/data
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      TIERPLAN_PUBLIC_URL: https://tierplan.example.com:7480
      TIERPLAN_ACME: "on"
      TIERPLAN_ACME_ACCEPT_TOS: "true"
      TIERPLAN_ACME_EMAIL: admin@example.com
      TIERPLAN_ACME_CHALLENGE: dns-01
      TIERPLAN_DNS_PROVIDER: cloudflare
      TIERPLAN_CLOUDFLARE_API_TOKEN_FILE: /run/secrets/cloudflare_token
    secrets:
      - cloudflare_token

secrets:
  cloudflare_token:
    file: ./cloudflare-token.txt

volumes:
  tierplan-data:
```

For HTTP-01, set `TIERPLAN_ACME_CHALLENGE` to `http-01`, leave out `TIERPLAN_DNS_PROVIDER`, the token and the `secrets` entries, and publish port 80 as shown in the comment.
