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

Agents do not need a trusted certificate. The install command you copy from Add environment carries the pin of the certificate the agent will see, and the agent connects only if the certificate matches. Every agent also keeps the self-signed pin it receives when it connects. That is why TierPlan can fall back to its own certificate without cutting agents off (see [Renewal, backoff, the restore pause and the fallback](#renewal-backoff-the-restore-pause-and-the-fallback)).

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

Certificate files win over Let's Encrypt. While they are set, Let's Encrypt is paused: TierPlan keeps its Let's Encrypt certificate, order and account, and makes no request to the CA. Remove both variables and restart to use Let's Encrypt again. TierPlan never renews certificate files. It tells you in the bell when they are close to expiring (see [What each bell warning means](#what-each-bell-warning-means)).

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

TierPlan deletes only the records it created, by the record ID it stored when it made them, never by searching for a name. If a delete fails, the record is left and retried on a backoff, and the bell tells you (see [What each bell warning means](#what-each-bell-warning-means)). The retries pause while Let's Encrypt is paused after a restore, or while certificate files are set. You can always delete a record yourself in the Cloudflare dashboard.

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
| `TIERPLAN_HSTS` | `on` or `off`. See [HSTS](#hsts). |

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

## HSTS

HSTS (the `Strict-Transport-Security` header) tells a browser to use only HTTPS for a host name. It is off by default, because a browser that holds a policy for a name cannot be talked out of it if the certificate later goes wrong.

Turn it on with the **Send HSTS** switch in **Settings > HTTPS**, or with `TIERPLAN_HSTS=on`. A variable locks the switch. TierPlan then follows these rules:

- It sends HSTS only on a response over a certificate that verifies for your public host. It never sends a positive lifetime with its own self-signed certificate, and it never adds `includeSubDomains` or `preload`.
- The lifetime grows in steps: 5 minutes, then 1 week, then 30 days, then 1 year. It moves up a step only after the current one has been sent for a while: 24 hours at 5 minutes, 1 week at 1 week, and 30 days at 30 days. A withdrawal, a certificate that stops being trusted, a new public host and a restore each start again at 5 minutes.
- For a certificate that lives less than 10 days, such as a short-lived Let's Encrypt certificate, the lifetime stops at 1 week.
- It withdraws HSTS ahead of the certificate's end. TierPlan then sends `max-age=0`, which makes a browser forget the policy, because a browser that kept it would refuse the self-signed certificate that takes over, with no way to click past the warning. The withdrawal starts 1 week before a 90-day certificate ends. For a short certificate the window is a third of its lifetime: 2 days before a 6-day certificate ends. When a renewal works, or the certificate files hold a new certificate, HSTS comes back and starts again at 5 minutes.
- **Stop using Let's Encrypt** is refused while HSTS is on. Turn HSTS off first, and wait for browsers to forget the policy before you stop. `TIERPLAN_ACME=off` is refused at start for the same reason.
- HSTS applies to the whole host name on every port. A browser that has seen it also refuses plain HTTP for any other service on that name, such as a site on port 80.

## Renewal, backoff, the restore pause and the fallback

### When TierPlan renews

TierPlan renews on its own, with nothing for you to press. It renews inside the window the CA names when the CA offers one. Otherwise it renews two thirds of the way through the certificate's life, or half way for a certificate that lives less than 10 days. **Renew now** does it at once and replaces the certificate in use without a restart.

### Backoff

When a run fails, TierPlan waits before it tries again: 1 minute, 10 minutes, 100 minutes, then 24 hours for every later failure. For a certificate that lives less than 10 days a wait is never longer than 6 hours, so it does not outlast the certificate. When the CA sends a Retry-After, TierPlan waits at least that long, up to 1 week, and a longer value is cut to that. After 3 failed runs in 1 hour, **Issue certificate** and **Renew now** are refused until the hour is up, so a mistake does not use up Let's Encrypt's limits.

### The pause after a restore

An instance restored from a backup makes no Let's Encrypt request until you press **Resume Let's Encrypt**. The restored copy may not be the install your host name points at, and two installs on one account would use up each other's limits. Check that your public URL points at this install, then resume. With `TIERPLAN_ACME=on`, TierPlan orders again as soon as you resume.

### The fallback

If renewals keep failing and the Let's Encrypt certificate expires, TierPlan serves its own self-signed certificate instead and keeps trying. Browsers warn about it. Agents stay connected, because each holds the self-signed pin. When a renewal works, the Let's Encrypt certificate comes back by itself.

### The host clock

TierPlan judges expiry, renewal and the fallback by its own clock. A clock that is wrong makes it renew late or early, so keep NTP running on the host. TierPlan reads the difference from Let's Encrypt's own responses whenever it talks to it: in every run, whatever the mode, and once after a start while Let's Encrypt is active. With no such response nothing is shown. TierPlan never queries an NTP server.

## What each bell warning means

The bell at the top of the page lists these for instance admins only. None can be dismissed: each goes by itself when its cause is gone.

| ID | Title | What it means | What to do |
|----|-------|---------------|------------|
| `https_cert_fallback` | The HTTPS certificate expired | The Let's Encrypt certificate expired because renewal kept failing. TierPlan serves its self-signed certificate, so browsers warn. Agents stay connected. | Fix the cause shown on the HTTPS page, then press **Renew now**. It goes when a renewal works. |
| `https_renewal_failed` | The certificate couldn't be renewed, or The certificate couldn't be issued | The last order failed. The warning names the error class and when the next attempt is. "Issued" means there is no Let's Encrypt certificate yet and `TIERPLAN_ACME=on` keeps trying. | Look the class up in [Troubleshooting by error class](#troubleshooting-by-error-class). It goes after the next run that works. |
| `https_acme_paused` | Let's Encrypt is paused | TierPlan makes no request. The reason is one of three: a restore, a Retry-After from the CA, or 3 failed runs in 1 hour. | After a restore, press **Resume Let's Encrypt**. For the other two there is nothing to do: it goes when the time shown has passed. |
| `https_clock_off` | The server clock is off | TierPlan's clock differs from Let's Encrypt's by more than 5 minutes. The warning says by how much and which way. A measurement older than 2 days is dropped. | Fix NTP on the TierPlan host. It goes with the first response that shows the clocks in step. |
| `https_cert_expiring` | The HTTPS certificate expires soon, or The HTTPS certificate has expired | The certificate in use, from Let's Encrypt or from certificate files, has less than 14 days left, or less than a third of its lifetime for a short-lived certificate. | For Let's Encrypt, open the HTTPS page to see why it has not renewed. For certificate files, replace them: TierPlan picks the new ones up within 1 minute. |
| `https_txt_left` | A DNS record was left behind | TierPlan could not delete a TXT record it created in your Cloudflare zone. It names the oldest and counts the rest. | Wait: TierPlan keeps retrying. If it stays, delete the record in Cloudflare. |

The clock warning does not stop anything: preflight, **Issue certificate**, the renewal time, the backoff and the fallback never read it. TierPlan never renews certificate files, so the expiring warning is how you find out that they need replacing. Let's Encrypt being paused because certificate files are set gives no warning, since you set it up that way.

## Troubleshooting by error class

When an order fails, the HTTPS page shows a headline and a fix. Both are copied here, with the names the page fills in written as words.

| Class | What the page says | What to do |
|-------|--------------------|------------|
| `rate_limited` | Let's Encrypt is limiting requests. | TierPlan waits until the time the page shows before it tries again. There is nothing to do. |
| `validation_unreachable` | Let's Encrypt couldn't reach http://your host/.well-known/acme-challenge/. | Port 80 on your host must reach TierPlan, for example with -p 80:7480. Check your firewall and router, then try again. |
| `validation_dns` | The DNS records for your host don't point at this host. | Make the A and AAAA records for your host point at this host. Remove an old AAAA record if you have no IPv6. Then try again. |
| `caa_forbids` | A CAA record forbids Let's Encrypt for your host. | Add a CAA record that allows letsencrypt.org, or remove the one that blocks it. |
| `identifier_rejected` | Let's Encrypt won't issue for your host. | Use a public DNS name under a domain you own as the public URL. |
| `contact_invalid` | Let's Encrypt rejected the contact email. | Register again with a valid address, or leave it empty. |
| `tos_action_required` | Let's Encrypt changed its terms. | Read the current terms at the address the page shows, then try again. |
| `account_gone` | Let's Encrypt doesn't know this account. | Register the account again in the Account section. |
| `profile_invalid` | Let's Encrypt doesn't offer the profile the profile. | Choose another profile under Advanced, or go back to the CA default. |
| `ca_unavailable` | Let's Encrypt couldn't be reached. | TierPlan tries again on its own schedule, next at the time the page shows. If it keeps failing, check this host's internet access. |
| `ca_untrusted` | TierPlan doesn't trust the certificate authority's certificate. | This only happens with a test directory. Check TIERPLAN_ACME_CA_FILE. A server clock that is badly wrong can cause it too. |
| `internal` | Something went wrong inside TierPlan. | Open Details and check the server log. TierPlan tries again at the time the page shows. |
| `dns_token_invalid` | Cloudflare didn't accept the API token. | The token is wrong, expired or revoked. Create a new token and enter it again under Validation method. |
| `dns_token_scope` | The Cloudflare token can't manage DNS records for your host. | The token needs both Zone:DNS:Edit and Zone:Zone:Read, and it must include the zone for your host. Fix it in Cloudflare, or enter a new token. |
| `dns_zone_not_found` | Cloudflare has no zone for your host. | Add the zone to the Cloudflare account the token belongs to, and check that the token includes it. Or make _acme-challenge.your host a CNAME to a record in a Cloudflare zone you can edit. |
| `dns_delegation_target` | _acme-challenge.your host is delegated to the target, which TierPlan can't edit. | The target must be in a Cloudflare zone this token can edit. Add that zone to the token, or change the CNAME. |
| `dns_not_visible` | The TXT record for your host didn't appear on the resolvers. | Check that this host can reach the resolvers on port 53. If you set resolvers under Advanced DNS, they must answer for public records. Clear them to ask the zone's own nameservers. |
| `dns_rate_limited` | Cloudflare is limiting requests. | TierPlan waits until the time the page shows before it tries again. There is nothing to do. |
| `dns_unavailable` | Cloudflare couldn't be reached. | TierPlan tries again on its own schedule, next at the time the page shows. If it keeps failing, check this host's internet access. |
| `validation_txt` | Let's Encrypt didn't find the right TXT record for _acme-challenge.your host. | TierPlan saw the record, but Let's Encrypt didn't. Check that the zone's nameservers at your registrar are Cloudflare's, and that nothing else at _acme-challenge.your host gets in the way, such as a conflicting CNAME. Then try again. |

## Let's Encrypt rate limits

Let's Encrypt limits how often an account can ask. These are its published limits for the production service, as of 2026-11-12. They can change, so check [Let's Encrypt's rate limits page](https://letsencrypt.org/docs/rate-limits/).

- 5 certificates for the same exact set of names per 7 days.
- 300 new orders per account per 3 hours.
- 5 failed validations per name per account per hour.
- 50 certificates per registered domain per 7 days.

A renewal that Let's Encrypt itself coordinates, which is how TierPlan renews when the CA offers renewal information, is exempt from all of them.

TierPlan keeps well inside the limits. Saving a setting never orders a certificate. A first order starts when you press **Issue certificate**, or by itself when `TIERPLAN_ACME=on`. **Renew now** names the certificate it replaces when the CA offers renewal information, so it counts as a coordinated renewal. Failed runs back off as described in [Renewal, backoff, the restore pause and the fallback](#renewal-backoff-the-restore-pause-and-the-fallback), and after 3 failed runs in 1 hour TierPlan refuses manual attempts. **Test token** places no order.
