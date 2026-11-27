<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# Ingress

Ingress lets a stack's compose file ask for a public hostname, and TierPlan runs a Traefik proxy that serves that
hostname over HTTPS: one on every node of a swarm, and one on a standalone host (a host with an agent, or TierPlan's own
local socket). This page covers the part you operate in this version: switching ingress on and off for an environment,
what that creates and what stops it, how existing stacks join, and what the proxy's access log records. The sections up
to [On a standalone host](#on-a-standalone-host) describe a swarm; that section says what differs on a standalone host. Every step on this page is an API call, and the Ingress page in the web interface covers them. Reading needs the
`ingress.read` permission on the environment (a Viewer has it) and changing needs `ingress.manage` (an Admin has it; see
[permissions.md](permissions.md)).

- [What switching on creates](#what-switching-on-creates)
- [Switching on](#switching-on)
- [What refuses the switch](#what-refuses-the-switch)
- [On a standalone host](#on-a-standalone-host)
- [TierPlan's own certificate and HTTP-01](#tierplans-own-certificate-and-http-01)
- [Existing stacks need a redeploy](#existing-stacks-need-a-redeploy)
- [TierPlan's own route](#tierplans-own-route)
- [The status](#the-status)
- [The join token](#the-join-token)
- [The access log](#the-access-log)
- [Switching off](#switching-off)
- [Nodes that were away](#nodes-that-were-away)
- [DNS](#dns)
- [Certificate Transparency](#certificate-transparency)
- [Let's Encrypt limits](#lets-encrypt-limits)
- [Virtual IP](#virtual-ip)
- [Where the virtual IP will not work](#where-the-virtual-ip-will-not-work)
- [Stable codes](#stable-codes)

## What switching on creates

Switching on creates four things in the swarm, all labelled `tierplan.component=ingress`, and none of them is yours to
edit: TierPlan shows them read-only and refuses to remove or change them while ingress is on.

| What | Name | Notes |
| --- | --- | --- |
| Join token secret | `tierplan-ingress-token-g<generation>-<environment>-<token>` | A swarm secret that holds the token each proxy enrols with. The token is not shown anywhere. |
| Overlay network | `tierplan-ingress` | A swarm overlay on a free /22 block of the swarm's address pools (or the subnet you give). It is not attachable, so a plain `docker run --network tierplan-ingress` is refused. That is not a hard barrier; do not rely on it for isolation. |
| Proxy service | `tierplan-ingress` | A global service, so one task runs on every node. It runs as user 1000, with no Docker socket and no extra capability. It publishes ports 80 and 443 of every node in host mode, so the proxy sees each client's own address, and its readiness port 7483 in host mode too (see [The readiness port](#the-readiness-port)). |
| Certificate cache volume | `tierplan-ingress-cache` | One local volume per node, where the proxy keeps the certificates and keys it was sent. |

The proxy image is `ghcr.io/ianglennon/tierplan-ingress`, at the same version as TierPlan. When TierPlan is
upgraded, TierPlan updates the service to the new image on its own: one node at a time, stopping the old task before
it starts the new one, and rolling back if the new task does not become healthy.

## Switching on

Two calls, both `POST`, both under `/api/env/{envId}/ingress`:

1. `enable/preview` says what would be created and what would stop it. It writes nothing.
2. `enable` runs the same checks again and, if nothing refuses, starts a job that makes the secret, the network and the
   service and then switches ingress on. It answers `202` with a job ID, and `jobs/{jobId}` reads the job.

The body of both is optional. `{"subnet": "10.0.8.0/22"}` asks for a block of your own choosing; without it TierPlan
takes the lowest free /22 of the swarm's pools.

The job ends when the objects exist. Each node's proxy then enrols and applies its first configuration on its own,
which takes a little while longer: read `GET .../ingress/status` to see when every node is ready.

If a step of the job fails, the steps after it are not run and nothing is rolled back. The status then reports `enableFailed` (and
the class `enable_failed`), and ingress stays off while what the job made stays in the swarm; it is not an incomplete
switch-off. Switching off removes it. Switching on again starts from
what is there. Only one operation runs on an environment at a time: while a stack deploy or another ingress job holds the
environment, the call answers `operation_locked` (409) and writes nothing.

## What refuses the switch

A refusal is a `409` with the problem `ingress_preflight_refused`, and each finding in its `errors` carries a stable
code. The preview shows the same findings, so you can see them before you switch. Nothing has been written when one
appears.

| Code | What is in the way |
| --- | --- |
| `ingress_no_manager` | No manager node's agent is connected, so TierPlan cannot write to the swarm. |
| `ingress_port_in_use` | A swarm service, in a stack or not, publishes TCP port 80, 443 or 7483 (in ingress mode or host mode), or a container on a node does. The finding lists each one. A UDP 443 publisher (HTTP/3) does not block. |
| `ingress_no_address_space` | The swarm's address pools have no free /22 block. |
| `ingress_subnet_invalid` | The subnet you gave is not an IPv4 CIDR block. |
| `ingress_subnet_overlaps` | The subnet you gave overlaps an existing network. The finding lists them. |
| `ingress_name_taken` | The swarm already holds a service, a network or a join token secret under a name ingress uses that is not TierPlan's. Rename or remove it. The check does not look at volumes: the certificate cache volume is a per node object, and a volume of that name that does not carry TierPlan's label would be mounted by the proxy, and switching off leaves it in place. |
| `ingress_url_unset` | TierPlan does not know its own address, which the proxies dial. Set the agent URL or the public URL. |
| `ingress_url_routed` | TierPlan's own address is a hostname that a route or a reservation holds, so the proxies would reach TierPlan through themselves. |
| `ingress_http01_conflict` | TierPlan runs on this swarm, its own certificate uses HTTP-01 and the carve-out cannot reach it. See [TierPlan's own certificate and HTTP-01](#tierplans-own-certificate-and-http-01). |
| `ingress_paused` | A restore paused ingress on this environment. An Admin resumes it first with `POST .../ingress/resume`. |

Two findings are warnings and never refuse. `ingress_nodes_not_checked` lists the nodes whose ports could not be read
(a node that is down does not block the switch). `ingress_http01_unknown` says TierPlan could not tell whether
[TierPlan's own certificate and HTTP-01](#tierplans-own-certificate-and-http-01) applies. The carve-out's
`ingress_http01_carve_out` is not a finding: the preview carries it in `notice`.

## On a standalone host

A standalone environment, whether its host runs a TierPlan agent or is TierPlan's own local socket, has one node, so
there is one proxy. The calls are the same as on a swarm: `enable/preview`, `enable`, `status`, `join-token`,
`access-logs`, `disable/preview` and `disable`. The preview says `creates.standalone` is true.

**What switching on creates.** The proxy is not a swarm service but one container of a compose project that TierPlan
owns on the host, all of it labelled `tierplan.component=ingress`:

| What | Name | Notes |
| --- | --- | --- |
| Compose project | `tierplan-ingress` | One service, the proxy container, with a restart policy so it comes back after a reboot. It publishes ports 80 and 443 of the host. |
| Bridge network | `tierplan-ingress` | A bridge on a free /24 block of the host's address pools (or the subnet you give). Routed services of every stack join it. |
| Cache volume | `tierplan-ingress-cache` | Where the proxy keeps the certificates and keys it was sent. |
| Join token | a file in the container | The token is written into the stopped container as a root-owned file with mode 0444, which the proxy, running as user 1000, can read. The container's root filesystem is not read-only, because Compose does not copy a secret into a read-only container. The token is not an environment variable, a bind mount or a swarm secret, and it is not kept anywhere TierPlan or you can read it back. |

The job ends when the container is up. The proxy then enrols on its own, and `GET .../ingress/status` shows it: the
objects `container` and `volume` say what the host holds (`service` and `secret` stay false on a standalone host), and
`nodes.expected` is 1. If the proxy has run for five minutes without enrolling, the status reports the class
`not_enrolled` in `lastEnsure`. That nearly always means the proxy cannot reach TierPlan's address; see
[the local socket](#the-local-socket) below. Nothing is recreated for it, because that would not change what the proxy can
reach. The job that brings the project up on a later pass, because the image changed or the container went missing, has the
kind `ingress_ensure`.

**Refusals.** The preview shows the same findings as on a swarm. These apply to a standalone host:

| Code | What is in the way |
| --- | --- |
| `ingress_host_unreachable` | The host's agent is not connected, so nothing can be created or removed on it. Reconnect it first. A standalone host has one node, so this refuses the switch and is not a warning. |
| `ingress_port_in_use` | Any container on the host publishes TCP port 80 or 443, a stopped one included, because a stopped container with a restart policy takes the port back at the next reboot. A UDP 443 publisher does not block. TierPlan's own container is caught here like any other. |
| `ingress_no_address_space` | The host's address pools have no free /24 block. |
| `ingress_subnet_overlaps` | The subnet you gave overlaps an existing network. |
| `ingress_name_taken` | The host already holds a container, network or volume under a name ingress uses that is not TierPlan's. Remove or rename it. |
| `ingress_name_shadowed` | Not a preview finding: the switch-on job checks it once it has made the ingress network, because only then is the network's ID known. A container on the host, in any state, is joined to a network by a name that is the start of that ID (such a name is made only of the digits 0-9 and the letters a-f). Docker reads a name it cannot find as the start of a network's ID, so after a reboot that container could join the ingress network under a name of its own. Ingress stays off and the job names the container. Disconnect it from that network or remove it and switch on again, or switch off and on again, which makes the network with a new ID. While ingress is on, every pass of the ensure loop runs the same check, and again after anything it brings up: on a hit the proxy is taken down and kept down while that network is there, and the status reports `name_shadowed` in `lastEnsure`; the server log names the container and the network name it is stored on. Once the container is disconnected or removed, the next pass brings the proxy back up. |
| `ingress_http01_conflict` | TierPlan's own certificate uses HTTP-01, the host is TierPlan's own host, and TierPlan cannot find its own container to connect it to `tierplan-self`, so the carve-out cannot reach it. TierPlan's own host includes the local socket environment, and an agent environment whose engine is the one TierPlan itself talks to. Switch TierPlan's own certificate to DNS-01 first (see [https.md](https.md)). When TierPlan finds its container, this is not a refusal: see [TierPlan's own certificate and HTTP-01](#tierplans-own-certificate-and-http-01). |

**Existing stacks join on their next deploy.** As on a swarm, switching on changes no stack. A stack that holds a route
is attached to the `tierplan-ingress` network by its next deploy, and the preview and the status list the stacks that
need one, whether or not they look attached, from what the host holds. Each routed service gets one alias on the shared
network, made from the stack and the service: lowercase, with a short hash of the pair at the end, for example
`shop-web-1a2b3c4d5e6f7a8b`. The bare service name, `web`, is not a name on the shared network. So two stacks that each have a
service `web` never answer for each other, and the proxy reaches a service by its alias. Inside its own stack a service
still uses its plain name, on the stack's own network.

The engine also answers a container's own name and its hostname on that network, and the alias is made from names
anyone can see. So a service that is on `tierplan-ingress` may not set `container_name` or `hostname`. A service is on it
when it has a route, when the file declares the network as external, or when its `network_mode` is `tierplan-ingress`.
The file is refused with `ingress_container_name` or `ingress_hostname` at that key. On the shared network a container
keeps the name TierPlan gives it, `<stack>-<service>-<number>`, and has no alias but its route alias. A file that names
the network by its ID instead of its name is held to the same rule when it is deployed, and the deploy fails. A service
off that network keeps its `container_name` and `hostname`.

Docker also finds a network by the start of its ID. So a network whose name is the start of the ingress network's ID
counts as the ingress network. A stack deploy may not create a network under such a name. Creating one, or connecting a
container to one, through the Networks page answers `409 ingress_owned`. Starting or restarting a container that would
end up on the ingress network under a name of its own, rather than the one TierPlan gives it, is refused with
`ingress_owned` too.

**The reserved stack name.** `tierplan-ingress` is TierPlan's own name for the proxy's project on a standalone host, and
a stack of that name would have the proxy removed as an orphan at its next deploy. Creating or importing from git a stack
by that name is refused with `422 stack_name_reserved`, and the name check answers `invalid` with the same sentence. The
proxy's own project is never listed as an unmanaged stack, so adopting by that name answers `404 unmanaged_stack_not_found`;
if something else shares the project name, it is listed as not adoptable, and adopting it answers `409 stack_not_adoptable`
with that sentence. On a swarm the name is free.

<a id="the-local-socket"></a>**The local socket.** TierPlan's own host works with no agent, and the proxy dials TierPlan from inside a container.
Two things must hold for it to enrol: TierPlan must listen on an address other than loopback (a listener on `127.0.0.1` is
not reachable from a container by any address), and the host part of the agent URL, which is the address the proxy dials,
must resolve inside a container to an address that reaches TierPlan. A hostname that the host itself resolves to a loopback
address, such as a `127.0.1.1` entry in `/etc/hosts`, resolves to the container's own loopback and is refused. Use the
host's LAN address or the bridge gateway in the agent URL, or a name that resolves to one. TierPlan adds no `extra_hosts`
mapping by default. The status stays at no enrolled node, and then reports `not_enrolled`, when this is wrong.

**The join token.** The proxy enrols once with the token it was given and then uses its own key. While ingress is on, a
rotation of the join token is refused with `409 ingress_token_not_enrolled` before the proxy has enrolled, and no token is
made, because the new token could not reach the container that already holds the old one. Once the proxy has enrolled, a
rotation makes the next token and changes nothing on the host. TierPlan makes a fresh token itself only when it creates or
starts the proxy container and no node is enrolled.

**Switching off.** The calls and the refusals for routes that are live and for a paused environment are the same as on a
swarm. A standalone host that is not connected refuses the switch-off with `ingress_host_unreachable` and nothing is
changed, because nothing can be removed from it, and a switch that said "off" would leave the proxy running. Removing the
environment itself is not affected. Otherwise the job stops serving routes, removes the proxy container, the network and
the cache volume, which holds the private keys, disables the join token and removes the proxy node. A network that another
container still uses is not removed: the job fails and names that container, and you switch off again once it is gone.

## TierPlan's own certificate and HTTP-01

The proxies take port 80 on every node, or on the host on a standalone host. That matters only when TierPlan runs in this
environment and its own certificate counts as HTTP-01, which is strict: an active Let's Encrypt configuration for TierPlan's
own certificate whose challenge is HTTP-01, and not paused. A self-signed install, your own certificate files and Let's
Encrypt with DNS-01 need nothing on port 80, and nothing in this section applies to them. When TierPlan runs outside the
environment, nothing conflicts and nothing is shown.

**The carve-out.** When it applies, the proxies keep TierPlan's renewals working. On port 80 they send TierPlan the requests
for `/.well-known/acme-challenge/` on TierPlan's own hosts, the host of the public URL and the host of the agent URL, and
nothing else: another host on that path, and TierPlan's hosts on any other path, get the usual redirect to HTTPS. Those
requests go over [`tierplan-self`](#the-tierplan-self-network) to TierPlan's port 7480, whose plain listener answers that path
only while an order is open (see [https.md](https.md#lets-encrypt-with-http-01)). The carve-out works whether TierPlan's own
route is on or not, so it needs the network, and TierPlan on it: on a standalone host TierPlan connects its own container, and
on a swarm TierPlan's service needs the stack-file lines (see [Turning it on](#turning-it-on)). The switch-on preview then
carries a notice, `ingress_http01_carve_out`, naming the hosts, and the switch goes ahead. A TierPlan that publishes port 80
itself for HTTP-01 (`-p 80:7480`) is refused with `ingress_port_in_use`: take that port off just before you switch ingress on.
After ingress is switched off, nothing answers on port 80 until you publish it to TierPlan again.

**When the switch is still refused.** Where the carve-out cannot reach TierPlan, the switch is refused with
`ingress_http01_conflict`, so the ingress never takes port 80 from a renewal without saying so. On a swarm where TierPlan runs
as a service that is not on `tierplan-self`, the refusal says:

> TierPlan runs on this swarm and its own certificate uses the HTTP-01 challenge, which needs port 80 that the ingress takes on
> every node. Add the tierplan-self network to TierPlan's service, or switch TierPlan's own certificate to DNS-01. Then turn
> ingress on again.

Where TierPlan runs on a swarm node as a plain container, which cannot be put on the network, the second sentence is only
"Switch TierPlan's own certificate to DNS-01." On a standalone host where TierPlan cannot find its own container, the refusal
ends "TierPlan couldn't find its own container to connect it to tierplan-self. Switch TierPlan's own certificate to DNS-01
first."

On a swarm where ingress is not on yet, TierPlan makes `tierplan-self` only once ingress is on, and ingress is refused until
TierPlan's service is on the network. Make the network yourself first, on a manager, with the labels that tell TierPlan it is
its own:

```sh
docker network create --driver overlay --label tierplan.component=ingress --label tierplan.ingress.role=self tierplan-self
```

Then add the stack-file lines, deploy again and switch ingress on; TierPlan takes the network over as its own.
[Running TierPlan inside a swarm](install-in-swarm.md#tierplans-own-ingress-route-optional) gives the steps. The other way is
to switch TierPlan's own certificate to DNS-01 (see [https.md](https.md)), which needs neither port 80 nor the network.

TierPlan decides where it runs from its own Docker socket: the cluster it reports (a manager), or its node (a manager or a
worker), against the environment's. When its certificate uses HTTP-01 and it cannot tell (no Docker socket is mounted, the
engine does not answer, or it is a worker whose node the environment does not list), the preview shows
`ingress_http01_unknown` and the switch goes ahead: check by hand that TierPlan does not run on a node of this swarm, or
use DNS-01.

## Existing stacks need a redeploy

Switching ingress on changes no stack. A service joins the `tierplan-ingress` network when the stack is deployed with
ingress on, and only a service that declares a route joins; the rest of the stack stays off the network. So a stack that
already asked for a route before you switched ingress on has to be redeployed once.

On that network a service has no alias, not even its own name: every routed service of every stack shares it, and the
proxy reaches each one by its `<stack>_<service>` name. A compose file that gives a service `aliases:` on
`tierplan-ingress` (declared as an external network) is refused with `ingress_network_alias`; put the aliases on a
network of your own.

The preview and the status list those stacks (`ingress_redeploy_needed`, a warning), and until a stack is redeployed
its routes read "not attached" on its page. The redeploy is yours to choose, because it replaces the stack's tasks.

## TierPlan's own route

The ingress can serve TierPlan itself on a hostname you choose, with a certificate of its own, in the environment TierPlan
runs in. It is optional, and it is one route per install. Direct access on port 7480 keeps working whether it is on or off.
The switch is the card "TierPlan's own route" on the Settings tab of the Ingress page, which only the environment that
carries TierPlan shows. The codes below are explained in [Stable codes](#stable-codes).

### What it does

The route is a claim on a hostname, held by TierPlan itself rather than by a stack. The rules for a stack's hostname apply
to it: the name must be under a domain registered for the environment on the Domains tab, it must not be reserved or held by
anyone else, and its certificate is ordered with DNS-01 through that domain's token, like any route's. While the route is on,
a stack cannot claim the name, and deleting its domain is refused with `ingress_domain_in_use`, naming "TierPlan itself" among
the holders and ending "Turn off TierPlan's own route first."

The proxies reach TierPlan on its own port 7480 over HTTPS, and they check TierPlan's certificate by name (the host of the
public URL) against a trust bundle that follows every change of TierPlan's own certificate. The route serves only once its
certificate is valid and TierPlan is on the `tierplan-self` network. Its state is one of: off, `serving`, `pending` (rolling
out to the proxies), `waiting_certificate`, `self_not_attached` and `unknown`. The card, the routes list (as a route whose
owner is the system), `GET .../ingress/status` (`selfRoute`) and a read-only line on Settings > HTTPS all show it.

The public URL is not changed. While the route is on, TierPlan also accepts changes from `https://<route host>`, an origin it
takes from the stored route and never from a header (see [reverse-proxy.md](reverse-proxy.md#the-public-url-decides-everything)).
The agent URL's origin is accepted too whenever an agent URL is set, with the route on or off, so if you later set the
public URL to the route host the direct address keeps working (see [What keeps working on 7480](#what-keeps-working-on-7480)).

### The tierplan-self network

The proxies and TierPlan reach each other over a network of their own, `tierplan-self`. TierPlan makes it when the route is
switched on, or when ingress is switched on and the carve-out needs it (see the next section), and labels it
`tierplan.component=ingress` and `tierplan.ingress.role=self`. On a swarm it is an overlay that is not attachable, so a
plain `docker run --network tierplan-self` is refused; on a standalone host it is a bridge. Only the proxies and TierPlan
belong on it. A stack TierPlan deploys cannot reach it: a compose file that declares a network of that name, points at it
as an external network or names it as a network mode is refused with `self_network_reserved`, whoever deploys it. The one
exception is the swarm stack TierPlan's own service runs in, which may name it as an external network (the lines below).
Each ensure pass also lists what is on the network. Anything other than the proxies and TierPlan, a service or container
someone joined by hand, is named on the card and in the server log with `self_network_foreign_member`, and while it is
there TierPlan believes no forwarded address from the network. A network called `tierplan-self` without both labels is not
TierPlan's: TierPlan never attaches, adopts or trusts it, and logs `self_network_foreign`.

The network is what lets TierPlan believe the client address the proxy sends. While TierPlan is on it, its subnet, minus its
gateway, is added to the trusted proxies, without `TIERPLAN_TRUSTED_PROXIES`. The connection TierPlan sees comes from inside
that subnet (on a swarm, from the serving node's load-balancer address on the network), and Traefik writes the client's real
address into `X-Forwarded-For` itself, ignoring any such header that came in. The shared `tierplan-ingress` overlay is never
trusted, and that is why the route does not use it: every routed service of every stack is on that overlay, so any of them
could send TierPlan a forged `X-Forwarded-For` and choose the address that sign-in throttling and the audit log record. The
separation is not a hard barrier: on a node that already holds the network, Docker does not refuse an engine-level connect.
See [threat-model.md](threat-model.md#tierplans-own-route).

### Turning it on

Who may: an instance admin, who holds `ingress.manage` on every environment. The route changes which origins and proxy
addresses the whole install trusts, so an Admin of the environment alone is refused with `instance_admin_required`. Ingress
must be on in the environment; while it is off the check refuses with `vip_ingress_off`.

Press **Turn on own route** and give a hostname, or call `POST /api/env/{envId}/ingress/self/preview` and then
`POST /api/env/{envId}/ingress/self`, both with `{"hostname": "tierplan.apps.example.org"}`. The second answers `202` with a
job ID, read at `.../ingress/jobs/{jobId}`. The preview runs five checks and lists them in this order:

1. **Hostname.** Free and under a registered domain. A name that is the host of the agent URL, or of the public URL when no
   agent URL is set, is refused with `ingress_host_is_agent_url` (see [Agents and proxies](#agents-and-proxies)).
2. **Domain and certificate.** Valid, or a warning (`self_certificate_pending`): TierPlan orders the certificate when you switch
   on, and the route waits for it. Refused while Let's Encrypt's terms are not accepted (`self_certificate_terms`) or
   certificate work is paused after a restore (`self_certificate_paused`).
3. **Self network.** Whether TierPlan is, or will be, on `tierplan-self`. Refused with `self_route_not_here` when TierPlan runs
   in another environment or cannot tell which one, and with `self_route_needs_service` when TierPlan runs on a swarm node as a
   plain container. A swarm service that is not on the network yet is a warning with the lines to add (below).
4. **Origin.** The origin that will be added, `https://<hostname>`.
5. **Port 80 for TierPlan's certificate.** Only when TierPlan's own certificate uses HTTP-01: the carve-out (see the next section).

The switch is also refused while the public URL has a path (`self_route_base_path`), while neither a public URL nor an agent
URL is set (`self_route_no_url`) and while the install already has its route (`self_route_already_on`). The job's steps are
`self_check`, `self_claim` and `self_network`. It ends done when the route is stored, even if the route then waits in
`self_not_attached` or `waiting_certificate`.

**On a swarm**, TierPlan does not change its own service: an update would stop the task that sent it, and your next
`docker stack deploy` would drop a network TierPlan had added. You add the network instead, **after** switching the route on,
because TierPlan makes `tierplan-self` then, and a stack deploy that names an external network that does not exist fails. Until
you do, the route reads `self_not_attached` and serves nothing. Add these lines to the stack file TierPlan was deployed from,
the `networks:` entry under TierPlan's service and the `networks:` block at the top level, and run `docker stack deploy` again
on a manager:

```yaml
services:
  tierplan:
    networks:
      - tierplan-self
networks:
  tierplan-self:
    external: true
```

A service with a `networks:` entry leaves the stack's default network, so list the networks it already uses as well
(`- default` for the default one). The card shows the same lines with your service's name. For a TierPlan service made with
`docker service create`, not from a stack file, run this on a manager instead, with your service's name:

```sh
docker service update --network-add tierplan-self tierplan
```

Do not use the command for a service deployed from a stack file: the next `docker stack deploy` takes the network off again.
[Running TierPlan inside a swarm](install-in-swarm.md#tierplans-own-ingress-route-optional) has the steps in order.

**On a standalone host**, TierPlan connects its own container to `tierplan-self` itself, and the ingress ensure loop connects it
again within a pass when the container is recreated, for example by an upgrade. When TierPlan cannot find its own container
the switch is refused with `self_route_not_here`.

A TierPlan that still publishes host port 80 itself (`-p 80:7480`, or port 80 of a swarm service) makes switching ingress on
refuse with `ingress_port_in_use`, because the ports check counts TierPlan's own container like any other. Take the port off
before you switch ingress on.

### What keeps working on 7480

Direct access on port 7480 keeps working for every request, state-changing ones included, whether the route is on, waiting,
broken or off. The route only adds an address. The route is released from there: switching it off
is a plain call on the API that never goes through the route, and nothing refuses a release, even one that arrives through
the route itself, which may be cut off as the route goes. Switching ingress off for the environment releases the route first,
as the `self_route` step of the job, before the proxies go.

TierPlan accepts changes from the public URL's origin, from the agent URL's origin whenever an agent URL is set, and, while
the route is on, from the route's. Settings allow the public URL to be the route host only while an agent URL that the
ingress does not serve is set, and the agent URL's origin stays in the set whether the route is on or off, so a browser on
it can always change anything and sign in, switching the route off included, and after the route is gone it can move the
public URL back. All of these come from TierPlan's stored settings and the stored route, never from a header.

The card's **Direct address** is that address: the public URL's origin while the public URL is not the route host, and the
agent URL's origin once it is. Changes from it are always accepted, and so are changes from the agent URL's origin. With
neither URL set it is the address the page is open on.

### Agents and proxies

Agents and ingress proxies never connect through the route, because the route ends TLS at the proxy and the connect's
certificate pin and channel binding would not hold. Three places make sure:

- **Settings refuse it.** The agent URL can never be a hostname the ingress serves (`agent_url_is_ingress_host`). The public
  URL can be the route host only while an agent URL that the ingress does not serve is set (`public_url_is_ingress_host`). A
  claim, the route's or a stack's, is refused for the host agents and proxies dial (`ingress_host_is_agent_url`).
- **The route has no connect paths.** It leaves out `/agent/connect` and `/ingress/connect`, so on the route host the proxy
  answers 404 for them.
- **The handshake refuses it.** A connect that reaches TierPlan from the self network is refused with `self_network_peer`, which
  the agent's or the proxy's log shows. Point agents at the agent URL.

### Sign-in

Cookies are per host: a session made on the route host is not sent to the direct address, or the other way round, so you sign
in on each address separately. Single sign-on returns to the public URL, so a sign-in through your identity provider ends on
the public URL whichever address it started from.

### Folding and removing

A fold of the Local environment is refused while TierPlan's own route is on there (`fold_ingress_active`): the route is a
hostname that the fold would delete, and ingress is on. See
[Fold a host that has ingress set up](install-in-swarm.md#fold-a-host-that-has-ingress-set-up).

To switch the route off, press **Turn off own route**, or call `POST /api/env/{envId}/ingress/self/off/preview` and then
`DELETE /api/env/{envId}/ingress/self`, which answers `202` with a job ID (`self_route_not_on` when the route is not on in that
environment). The job's steps are `self_release` and `self_network`. The preview says whether `tierplan-self` is removed or
stays because the carve-out uses it. A stack operation running in the environment does not refuse the switch-off: the job
waits up to 30 seconds for the environment's operation lock, then releases the route without it.

On a swarm, the network stays while TierPlan's service is still on it. Take the stack-file lines out after switching the route
off and deploy again, before any other deploy of the stack, so that the stack file never names a network that is gone; a later
pass of the ensure loop then removes the network. If the preview says the carve-out uses the network, keep the lines. On a
standalone host TierPlan disconnects its own container and removes the network itself.

Switching ingress off releases the route first and then removes `tierplan-self`, or keeps it (`tierplan_attached`) while
TierPlan's own task or container is still on it. Nothing removes it later, because the ensure loop does not run while ingress
is off. Take the lines out, deploy, and then remove it on a manager with `docker network rm tierplan-self`.

## The status

`GET /api/env/{envId}/ingress/status` says whether ingress is on and paused, whether the access log is on, which of the
network, the service and the secret the swarm holds, and how many proxies are expected (one per node), enrolled and
ready (enrolled, healthy and running the current configuration). It also reports the result of the last pass that checks
the objects, the number of certificate cache volumes still waiting to be wiped (see below) and the stacks to redeploy.

TierPlan checks the objects once a minute and whenever the ingress state changes, and puts right what differs: a
missing secret, network or service, or a service that runs a different image than TierPlan's version. It writes only
when something differs, and never while another operation holds the environment. It reads the switches and the swarm again
once it holds the environment, so a switch-off or a restore's pause that comes in while it waits is not written over.

Two results of a pass mean it left the proxy service alone on purpose. `url_unset`: TierPlan has no usable address for the
proxies to dial (the agent URL and the public URL are both unset, or are not `https://` addresses), so nothing is written and
the running proxies keep the spec they have. `update_failed`: the swarm rolled back the last update of the proxy service, so
the same update is not sent again every minute (each try restarts one node's proxy first). It is tried again when the spec
TierPlan wants changes, and after an hour otherwise.

## The join token

The proxies enrol with a join token that the deployer keeps as a swarm secret. `POST /api/env/{envId}/ingress/join-token`
rotates it, and on a swarm whose ingress is on it stores the new token as that secret in the same call, under the
environment's operation lock. The deployer then moves the proxy service to the new secret and removes the one before, and does
not make a token of its own, so the token you were given is the one that enrols. A rotation is refused, and no token is made,
while another operation holds the environment (`operation_locked`), while a restore's pause stands (`ingress_paused`) and while
no manager is connected (`ingress_no_manager`). If the swarm refuses the secret the answer is `502 ingress_token_not_stored`
and no token is handed out. Nodes that are enrolled already do not use the token, so a rotation changes nothing for them. While
ingress is off the route only makes the token; switching on then makes its own.

## The access log

The proxy writes no access log until you switch it on. It is off by default.

    PUT /api/env/{envId}/ingress/access-logs     {"enabled": true}

The answer says when it applies: `rolling` (the proxy service is updated, one node at a time, so each proxy restarts in
turn), `recreate` (a standalone host: the next pass recreates the proxy container with the setting changed), `when_enabled` (stored now, applies when ingress is switched on) or `when_resumed` (stored now, applies when an
Admin resumes a paused environment).

What it records. The log is JSON, one line per request, written to the proxy's standard output. Each line carries the
**client address** and the **full request path, with the query string**, together with the host, the method, the status
and the timings. It never records any request or response header: the headers are dropped, so the value of an
`Authorization` or `Cookie` header is never logged. A query string can hold anything an application puts in a URL,
such as a token in a link, so switch the log on knowing both the addresses and the paths are written.

Where it goes and how much space it takes. The proxy service always sets its own log driver: `json-file` with
`max-size` of 10m and `max-file` of 3, which is at most 30 MB of log on each node. This bounds the space even where the
daemon's default driver has no limit. It also means a daemon that is set to send container logs elsewhere (Loki, for
example) is overridden for this one service, which TierPlan owns. The log stays readable with `docker service logs
tierplan-ingress` and in TierPlan's own service log viewer. If you want the proxy's log in your log system, collect
it from the nodes' `json-file` files.

## Switching off

Two calls again: `POST .../ingress/disable/preview` and `POST .../ingress/disable`. The preview lists what would be
removed, which nodes are not reachable now, and what would refuse.

Switching off is refused while any hostname is claimed with routes in the environment, a stopped stack's included
(`ingress_routes_live`). The refusal lists each hostname with its stack and path (for a stack you cannot see, only that
it is held). Clear it by removing the route labels and redeploying the stack, or by removing the stack. It is also refused
with `ingress_paused` while a restore's pause stands.

When nothing refuses, the job:

1. switches delivery off and closes the proxies' sessions, so no proxy is still being served;
2. removes the proxy service and waits, for up to two minutes, until its tasks are gone;
3. removes the certificate cache volume on each node of the swarm through that node's agent (the swarm's own node list is read, so a node that has no agent is included);
4. removes the network and the join token secrets;
5. disables the join token and removes the proxy nodes from the list. A node you revoked stays in the list as revoked, so that switching ingress off and on again does not let that host back in.

A step that fails stops the job, and you switch off again to carry on. Certificates are not touched: they belong to the
hostnames, not to the proxies.

## Nodes that were away

The cache volume holds the private key of every certificate the proxy served, so switching off must not leave it behind
on a node that was down or that has no agent. A node whose agent is not connected when you switch off, and a node of the
swarm that has no agent at all, is named in the preview and in the job and its volume is kept as a **pending wipe**. If
the swarm's node list cannot be read the step fails, so no node is left out unseen. The volume is removed as soon as that
node's agent connects (or is installed and connects), and the entry is
cleared and audited (`ingress.wipe_queued`, `ingress.wipe_done`). The status counts the pending wipes. Switching off
again retries the ones that remain.

A node that still runs a proxy container is never wiped. If you switch ingress on again before a pending wipe ran,
the entry is dropped without wiping, because the proxy owns the volume again. A node that left the swarm for good
keeps its entry, since there is no agent to reconnect.

## DNS

Every hostname a stack asks for gets its own certificate, and TierPlan proves it owns the name with the DNS-01 challenge.
It writes a TXT record at `_acme-challenge.<hostname>` through the token of the domain the hostname belongs to, waits for
Let's Encrypt to see it, and deletes the record afterwards. The token is the one you give for the domain; see
[https.md](https.md#the-token) for how to make a Cloudflare token and what it needs.

**Keep the apps domain apart from your main records.** A Cloudflare API token is scoped to accounts or to zones. It cannot be
limited to some records of a zone or to a subdomain. A token that can edit the DNS of the zone your apps live in can edit
every other record in that zone too. So give the apps their own registered domain, or at least their own zone, and make the
token for that zone alone. Then a leaked token reaches only the apps' records, and not your organisation's mail, website or
other records.

### CNAME delegation (advanced)

If you would rather keep the hostnames in a zone you do not want to give a token for, delegate only the challenge. In the
hostname's zone, make `_acme-challenge.<hostname>` a CNAME to a name in a second zone, the challenge zone, that you do
control. TierPlan follows the CNAME and writes the TXT record at its target. The token must see both zones: the zone of the
hostname, which TierPlan checks when you add the domain, and the challenge zone, where the record is written. A token that
sees only one of them is refused, with `dns_zone_not_found` or `dns_delegation_target`. The tests run against a fake DNS
provider and a test CA. This path is covered by automated tests and has not been run against a live second zone.

## Certificate Transparency

Every certificate TierPlan obtains is published in public Certificate Transparency logs, with every name in it. Anyone can
search those logs, and they cannot be edited afterwards. This includes a name that is requested ahead of the stack that
uses it: if you add a hostname to a domain, or a stack asks for one, the certificate for it is logged as soon as it is
issued, whether or not anything ever serves it. Do not put a name you need to keep private in a hostname, and do not
request one to try it out.

## Let's Encrypt limits

Let's Encrypt limits how often an account can ask. These are the limits as Let's Encrypt states them on 2026-08-05, for the
production service. They can change, so check [Let's Encrypt's rate limits page](https://letsencrypt.org/docs/rate-limits/).
[https.md](https.md#lets-encrypt-rate-limits) lists the same limits for TierPlan's own certificate.

- 300 new orders per account per 3 hours.
- 50 new certificates per registered domain per 7 days.
- 5 certificates for the same exact set of names per 7 days.
- 5 authorization failures per name per account per hour.
- A renewal that Let's Encrypt itself coordinates (ARI) is exempt from all of them.

TierPlan stops before the limit, because it cannot see the certificates that other tools issue under the same registered
domain. It starts no new-name order past these caps. A renewal is exempt from them, as it is from Let's Encrypt's own:

| What is counted | Let's Encrypt's limit | TierPlan stops at | Why it is lower |
|---|---|---|---|
| New certificates per registered domain in 7 days | 50 | 40 | Leaves room for other tools that order under the same domain. |
| Certificates for one hostname in 7 days | 5 | 4 | Keeps one certificate in hand for a name that has to be replaced. |
| Orders started by the account in 3 hours | 300 | 240 | Leaves room for orders the account starts that TierPlan does not count. |

A failed order backs off before it is tried again: 1 minute, 10 minutes, 100 minutes, then 24 hours for every later
failure, and a Retry-After from the certificate authority is honoured, up to a week. See
[https.md](https.md#backoff). After 3 failed runs in 1 hour a manual attempt is refused until the hour is up.

Certificates from Let's Encrypt's staging directory are only for testing that your DNS and token work. A browser does not
trust them, and TierPlan never serves one on a live route. A hostname whose certificate is from staging shows "DNS proven on
staging", which says only that the DNS challenge worked.

### Why a certificate is waiting

A certificate that is not ordered yet carries one of these reason codes, with the figures and the next slot when TierPlan
knows them:

| Code | Meaning |
|---|---|
| `no_account` | There is no ACME account yet: accept the terms of service first. |
| `rate_limit_new` | The registered domain has used TierPlan's cap of new certificates for the week. |
| `rate_limit_duplicate` | The hostname has been issued 4 times in the week. |
| `rate_limit_orders` | The account has started its cap of orders in the last 3 hours. |
| `ca_rate_limited` | Let's Encrypt answered a new-name order of the registered domain with a rate limit and a Retry-After, so TierPlan waits for it. |
| `concurrency` | Two certificates are already being ordered, the most TierPlan runs at once. The next one starts when a slot is free. |

## Virtual IP

A virtual IP gives a swarm's ingress one floating IPv4 address. keepalived runs on every node as a global service, the nodes
elect one holder with VRRP, and the holder answers for the address. You point DNS or your router at that one address, and it
keeps working when a node goes away. TierPlan is not in the path: if TierPlan is down, the address still moves. A node holds
the address only while its own proxy is ready, so a node whose proxy has stopped, or has no routes yet, does not take it.

The virtual IP is for swarms. A standalone host has one node and its own address already reaches its proxy, so the switch-on
is refused there with `vip_swarm_only`. Like the rest of this page, it is an API call in this version. Every route below is
under `/api/env/{envId}/ingress/vip`. The status and the moves need `ingress.read` on the environment and the changes need `ingress.manage`.

- [What it needs](#what-it-needs)
- [What the opt-in gives away](#what-the-opt-in-gives-away)
- [Switching the virtual IP on](#switching-the-virtual-ip-on)
- [What refuses the virtual IP](#what-refuses-the-virtual-ip)
- [Changing it](#changing-it)
- [Switching the virtual IP off](#switching-the-virtual-ip-off)
- [The status, the moves and the bell](#the-status-the-moves-and-the-bell)
- [Draining the node that holds it](#draining-the-node-that-holds-it)
- [How fast it moves](#how-fast-it-moves)
- [The router ID, and other routers on the segment](#the-router-id-and-other-routers-on-the-segment)
- [Interfaces, addresses and the address check](#interfaces-addresses-and-the-address-check)
- [Not supported yet](#not-supported-yet)
- [The readiness port](#the-readiness-port)
- [The keepalived image](#the-keepalived-image)
- [Residual risks](#residual-risks)

### What it needs

- **Ingress is on for the environment, and not paused.** The address points at the proxies, so with ingress off the switch-on is
  refused with `vip_ingress_off`, and while a restore's pause stands with `ingress_paused`.
- **Every manager's agent allows privileged services.** Set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` on the agent of
  every manager of the swarm. The next section says why and what it gives away. A manager counts as allowing when its connected
  agent reports the setting, or when TierPlan reaches it through its own local Docker socket, because whoever mounted that
  socket has already given TierPlan root there. A manager with no connected agent never counts as allowing. Workers need no
  setting, because the service is created through a manager and swarm starts the tasks. TierPlan never sets the opt-in itself:
  it is the host owner's setting on each agent, and the agent still enforces it.
- **Multicast between the nodes.** The nodes find each other with VRRP adverts sent to the multicast address 224.0.0.18. If your
  network filters multicast between the nodes, see [Not supported yet](#not-supported-yet) and the residual risks below.
- **One interface per node in the address's network.** See [Interfaces, addresses and the address check](#interfaces-addresses-and-the-address-check).
- **A free address, with its prefix length.** For example `192.168.1.50/24`. Choose one that no machine uses and that your
  DHCP server will not hand out. The prefix is required, and every node's address in the swarm must be inside it.

### What the opt-in gives away

The preview always carries this text, and this page carries it in the same words:

> The virtual IP needs `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` on every manager's agent. That setting lets TierPlan run privileged, host-network services on every node of this swarm, so a compromised TierPlan could take over every node. VRRP authentication only keeps honest neighbours apart; anything on this network segment can still contend for the address.

The keepalived service runs on the host network with added capabilities, which the agent's host policy treats as host access
(see [agent.md](agent.md#service-specs-and-host-access)). Without the opt-in, a manager's agent refuses that service, and
TierPlan refuses the virtual IP first with `vip_privileged_not_allowed`. With it, TierPlan, or anyone who controls TierPlan,
can start any privileged service on every node of the swarm, not only this one. That is the trade. If you do not accept it, do
not set the setting, and do not use the virtual IP. A swarm that already allows privileged services for host-folder volumes
has already made this choice.

The second sentence is about the network, not TierPlan. VRRP authentication stops a neighbour that does not hold the key from
taking part in the election. It does not stop a machine on the same segment from answering ARP for the address, or from
claiming it with its own router.

### Switching the virtual IP on

Two calls, in the house pattern of ingress itself. The body of both is `{"address": "192.168.1.50/24"}`, with an optional
`"routerId"` from 1 to 255 (see [the router ID](#the-router-id-and-other-routers-on-the-segment)).

1. `POST /api/env/{envId}/ingress/vip/preview` runs every check and writes nothing: no key is made and no secret created. It
   answers `200` whether or not the checks refuse. It lists the findings (each with a code, a sentence, the items it is about and
   a severity: `error` refuses, `warning` does not), the address as parsed, the router ID it would use and the one derived from
   the swarm, the privileged-services state of every manager, the fixed warning above, and the steps the job would run.
2. `POST /api/env/{envId}/ingress/vip` takes the environment's operation lock, runs the same checks again and answers `202` with
   a job ID, which `GET /api/env/{envId}/ingress/jobs/{jobId}` reads. The job makes the VRRP key, stores it as a swarm secret
   (`tierplan-keepalived-key-g<generation>-<envtag>`, where the tag is made from the environment's ID), creates the global service `tierplan-keepalived` and then switches the
   virtual IP on. The switch-on is audited as `ingress.vip_enabled` with the address, the router ID and the key's generation.
   The key itself is never shown, logged or audited, and a backup holds it only sealed.

The job ends when the service exists. The nodes then start, elect a holder and report; read `GET .../ingress/vip` to see one
holder appear, normally within about ten seconds of the tasks running.

### What refuses the virtual IP

A refusal is a `409` whose problem type is the finding's code, whose detail is the server's sentence, and whose `errors` list
each refusing finding with its items. A request that is not an address with its prefix length is a `422`. The preview shows the
same findings. Nothing is written when one appears.

| Code | What is in the way |
| --- | --- |
| `vip_swarm_only` | The environment is a standalone host. |
| `vip_ingress_off` | Ingress is off for the environment. |
| `ingress_paused` | A restore paused ingress on this environment. |
| `vip_already_enabled` | The environment already has a virtual IP. Change it or switch it off instead. |
| `vip_privileged_not_allowed` | Some manager does not allow privileged services. The detail names `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true`, and the items list every manager with `allows`, `refuses` or `unknown` (no connected agent, or the managers could not be read). |
| `vip_address_invalid` (`422`) | The address has no prefix length, or is not an address, or its prefix length is not from 8 to 32. A router ID outside 1 to 255 is refused as a malformed request, also `422`. |
| `vip_removal_pending` | A change was asked for a virtual IP whose switch-off failed part way. Switch it off again to finish removing it. |
| `vip_ipv6_unsupported` | An IPv6 address. See [Not supported yet](#not-supported-yet). |
| `vip_not_unicast` | The address is loopback, link-local, multicast or unspecified. |
| `vip_is_network_or_broadcast` | The address is the network or broadcast address of its prefix. |
| `vip_outside_node_subnet` | Some node's address is not inside the prefix. The items list those nodes. |
| `vip_is_node_address` | The address is already the address of a node. |
| `vip_in_use_by_environment` | The address is already the virtual IP of another environment of this install. |
| `vip_overlaps_docker` | The prefix overlaps the subnet of a network that exists on this swarm. |
| `vip_router_id_in_use` | Another environment of this install has a virtual IP with the same router ID on an overlapping network. The sentence names it. Set a different router ID. |
| `vip_name_taken` | The swarm holds a service named `tierplan-keepalived`, or a secret under the key secrets' prefix, that is not TierPlan's. It is never taken over. Rename or remove it. |
| `operation_locked` | Another operation holds the environment. The detail names it. |

Two findings are warnings and never refuse. `vip_inside_pool` says the address sits inside a range Docker may hand to networks,
so a network created later could take it. `vip_unverified` is always present: TierPlan cannot tell from the server whether the
address is unused, so each node checks when it starts (see [the address check](#interfaces-addresses-and-the-address-check)).

### Changing it

`PUT /api/env/{envId}/ingress/vip` takes the whole configuration, as the preview does: an address, and a router ID or none. Leaving
the router ID out derives it again. It is previewed through the same `POST .../ingress/vip/preview`, which then answers with
`change` true, the `current` address and router ID, a `strategy` and a `notice` that says what the change does to the address.
It needs every manager to allow privileged services, like the switch-on, and answers `404 vip_not_enabled` when the environment
has no virtual IP. The change is audited as `ingress.vip_changed`. The key and its secret are kept: a change never rotates them.

- **An address change rolls** (`roll`). The service is updated one node at a time, stopping the old task first, with ten seconds
  between nodes. One node holds the address at any time, and it is without a holder for about a second when the holder is replaced.
  The address clients must use changes during the roll, so the address may be briefly unanswered.
- **A router ID change, or both at once, replaces** (`replace`). The service is removed and created again. A roll would leave
  nodes on the old router ID and nodes on the new one electing their own holders, and both would answer for the address until
  the roll ended, so the job removes the service first and the address is unanswered for a few seconds.
- **No change** (`none`) touches no node.

The job waits until every node runs the new configuration, about a minute for three nodes and at most ten minutes, and only
then saves the new address. Until it is done the status shows the previous one and no other operation starts on the environment.
A paused node is not waited for, and the job's output names it: an address change leaves it on the previous address, and a
router ID change without keepalived, until it is active again.
A wait that runs out of time, or is cancelled, still saves the new address, because swarm carries on with the nodes that are left.
An update that swarm stopped, because a node could not run it, is not saved and swarm puts the previous address back. If a node
cannot run that either, swarm gives up and nodes may serve different addresses. The ensure loop then writes the previous
configuration again, and once an hour while that fails too; `lastEnsure` reports `update_failed` meanwhile. When TierPlan crashes or is killed while it waits, nothing is saved and
the ensure loop puts the previous address back, so make the change again. A normal stop of TierPlan saves the new address.

If a change does not finish, the next `PUT` with the same body finishes it.

### Switching the virtual IP off

`POST /api/env/{envId}/ingress/vip/disable/preview` says what would be removed: the service, how many of its tasks run, and the
name of the key secret (never its content). `DELETE /api/env/{envId}/ingress/vip` answers `202` with a job that removes the
service, waits (for up to a minute) until every node has stopped keepalived and so released the address, removes the key's
swarm secret and switches the virtual IP off. It is audited as `ingress.vip_disabled`. Removing a service is no escalation, so
it needs no manager check. A step that fails leaves the virtual IP switched off, names what is left in the swarm and is finished
by switching off again.

Switching ingress off while the virtual IP is on removes the virtual IP first, as the job's first item, and the ingress
switch-off preview lists it. If the virtual IP cannot be removed, ingress is left on: a virtual IP with no proxies can never be
healthy, and nothing is left behind.

### The status, the moves and the bell

`GET /api/env/{envId}/ingress/vip` answers `404 vip_not_enabled` for an environment with no virtual IP, and otherwise:

- the address, the router ID and whether it is derived, the key's generation (never the key) and `enabled`;
- `holder`: `state` is `none`, `one` or `many`, with the node IDs, and `holderlessSince` when no node holds the address. `many`
  is a problem;
- each candidate node: its VRRP state (`MASTER`, `BACKUP`, `FAULT` or `STOP`), whether its proxy is ready, its keepalived task's
  state, its last fault and when TierPlan last heard from it;
- each manager's privileged-services state, `foreignRouter`, `clockSkewNodes`, `authWindowSeconds` and the result of the
  last ensure pass.

The faults a node reports are `no_interface`, `ambiguous_interface`, `address_in_use` and `probe_failing`. Like the proxies,
TierPlan checks the service, its secret and its image once a minute under the environment's lock, and puts right what
differs. If a manager stops allowing privileged services, it records that, raises a bell item and does not remove the service
that is running.

TierPlan learns all of this from the keepalived containers themselves. The helper in each container writes one JSON line to its
standard output for each event, and TierPlan reads those lines from each node's container log through that node's agent, every
two seconds. So a move shows within a few seconds while the node's agent is connected. If TierPlan is down, the address still
moves, and TierPlan reads the moves when it is back, as far as Docker kept the log.

`GET /api/env/{envId}/ingress/vip/moves` lists the moves of the address, newest first, `limit` (1 to 100, 50 by default) at a time,
with `next` as the cursor to pass as `before`. Each move has the time, the node the address left, the node it went to and a
`reason`: `holder_failed` (the holder went silent or its task is gone), `probe_failing` (its proxy was not ready),
`drain_handoff` (TierPlan handed it over before a drain), `disabled` or `unknown` (none of these explains it). The last 500
moves of each environment are kept.

Two things about that history may surprise you:

- **A switch-off usually stores no `disabled` move.** Removing the service stops keepalived on every node at about the same
  moment, so the other nodes are usually stopped before they could report taking the address. Any move stored while a switch-off
  runs has the reason `disabled`, but do not expect one.
- **A move is stored a few seconds after the holder changes.** Every node has the same priority, so when a holder goes away all
  the backups can claim the address in the same second, and the one with the highest address keeps it. TierPlan stores the move
  once one node has been the only claimant for two seconds, so a failover is one move to the node that kept the address. The
  first election after you switch the virtual IP on stores none. The status shows the holder at once. After TierPlan
  restarts it stores moves once every node's agent has been read, or after 90 seconds when one stays offline.

Bell items, for a caller who manages the environment's ingress: `vip_no_holder` (an error: no node has held the address for more
than ten seconds, except while a node's keepalived has run for under fifteen seconds, which is a normal first start),
`vip_multiple_holders`, `vip_holder_unheard` (TierPlan has not heard from the only holder for more than two minutes and has
heard another node all that time; the holder is still shown, because a lost agent does not prove that it let go of the address), `vip_moved` (one item per environment however many moves there were in the last hour, with the count; a
`disabled` move raises none), `vip_foreign_router`, `vip_clock_skew` and `vip_privileged_lost` (a manager whose agent allowed
privileged services now refuses).

### Draining the node that holds it

`POST /api/env/{envId}/nodes/{nodeId}/availability` with `drain` first hands the address over when that node holds it. TierPlan
stops the node's keepalived container through the node's agent, so keepalived gets SIGTERM, sends a priority-0 advert and
releases the address, waits at most ten seconds for another node to report holding it and then sets the availability. The drain
is always made, whatever happens. A drain of any other node, and a pause, does nothing extra, because a pause does not stop
tasks. Swarm starts the stopped keepalived task again, and it comes back as a backup and stays one until the drain removes it,
because a node that comes back never takes the address back.

The answer is `200` with `vipHandoff`, and otherwise `204`. Its `outcome` is one of:

| Outcome | Meaning |
| --- | --- |
| `moved` | Another node took the address. `holder` names it. |
| `no_candidate` | No other node was ready to take the address, so nothing was stopped. The address is lost with the node until another proxy is ready. |
| `agent_unreachable` | The node's agent is not connected, or the stop failed. The address may drop briefly when the node drains. |
| `timeout` | Nothing took the address within ten seconds. |

The handoff is audited as `ingress.vip_handoff` with the node, the new holder (or none) and the outcome: a success for `moved`
and `no_candidate`, a failure with the outcome as its reason for the others. The impact read of the drain
(`GET .../nodes/{nodeId}/impact`) warns `holds_vip`, or `holds_vip_alone` when no other node is ready, so the confirm step shows it.

### How fast it moves

These figures were measured on three-node swarms of Docker 29 and 24 (Docker-in-Docker, so a software bridge), with the
advert interval at one second, and once on a real switch.

- **A failing proxy.** A holder whose proxy stops being ready goes to FAULT after its check fails twice and the address is on
  another node after 3.0 to 3.9 seconds. On the real switch, a neighbouring machine read the new holder's address after 3.9
  seconds.
- **A dead holder.** A node that is killed, or a keepalived that crashes, is replaced as holder after about 3.3 seconds, which
  is three advert intervals plus skew. The address is without a holder for that long, so expect three to four seconds.
- **A crashed keepalived leaves its address behind.** The container that was killed leaves the address on its interface until
  swarm starts its replacement, which clears it. A machine on the segment may still reach the dead holder until then. The
  service's restart delay is two seconds. On the real switch, with swarm's default five second delay, the neighbour read the
  right holder after 6.5 seconds. That is not re-measured with the two second delay. A node that dies as a whole leaves no address
  behind.
- **A node that comes back** does not take the address back (`nopreempt`), on the bridge and on the switch.
- **A change.** An address change leaves the address without a holder for about a second at a time, and a router ID change for a
  few seconds (see [Changing it](#changing-it)).

### The router ID, and other routers on the segment

Every VRRP group on a segment needs its own router ID, from 1 to 255. TierPlan derives one from the swarm's cluster ID: one
plus the first four bytes of the SHA-256 of the cluster ID, read as an unsigned number, modulo 255. The preview shows it as
`derivedRouterId` and `routerDerived` says whether the one in use is that one. To choose your own, send `routerId`.

- **Inside this install, a collision is refused.** Two environments whose virtual IPs share a router ID on overlapping networks
  are refused with `vip_router_id_in_use`, and the sentence names the other environment.
- **On the wire, it is a warning.** TierPlan cannot see other routers on your segment, so it cannot refuse in advance. Each
  node's helper reads keepalived's statistics every twelve seconds and reports a rising count of adverts for this router ID that
  fail authentication as a foreign router. The status shows `foreignRouter` with the sender's address, and the bell raises
  `vip_foreign_router`. It clears after three quiet polls. In the measurement a router with the same router ID and another key
  was reported about eight seconds after it appeared and cleared about 48 seconds after it was removed. The swarm's holder kept
  its address, and the foreign router held its own: both answer on the segment, each for its own address. Set a different
  router ID to stop it.
- **Clocks must agree.** The adverts are authenticated with `auth_hmac` in enforce mode with an anti-replay time window of 30
  seconds (`authWindowSeconds`). An advert whose stamp is older than the window by its sender's clock is refused, so the nodes'
  clocks must agree within 30 seconds. Run time synchronisation on every node. The status lists `clockSkewNodes`, and the bell
  raises `vip_clock_skew` for a node whose helper reports skew or whose agent's clock is 15 seconds or more away from the
  server's. TierPlan did not inject clock skew in its tests, so what a skewed node does to the election is keepalived's
  documented behaviour and not a measurement.

### Interfaces, addresses and the address check

**One interface per node, and no override.** The helper on each node looks at the node's own interfaces and keeps those that
have an IPv4 address inside the virtual address's network. It ignores Docker's own: `lo`, `docker0`, `docker_gwbridge` and
interfaces named `br-*`, `veth*`, `vx-*`, `vxlan*`, `ov-*`, `cali*`, `flannel*`, `cni*`, `tun*` or `tap*`. Exactly one must
remain. With none, the node reports the fault `no_interface`, and with more than one `ambiguous_interface`, naming the interfaces
it saw. Its task keeps running, it never holds the address, and the status shows the fault. There is no setting to choose the
interface in this version: change the node's networking so one interface qualifies. If an interface choice changes while the
service is running, the helper removes the address from the node's other interface when it starts, so the node never answers for
it on two.

**The address check.** The server cannot prove that an address is unused, so every node checks before it starts keepalived. It
sends three ARP probes for the address on its interface. If another machine answers, and no VRRP advert for this router ID is
heard for a few seconds, the node reports the fault `address_in_use`, stays out of the election and checks
again about every twelve seconds. When the other machine is gone, one node takes the address. A node restarting while a peer
holds the address is not flagged: the peer's adverts and its ARP answer are read as a peer. The check was run on Docker's
software bridge, with a plain container holding the address, and was reliable across restarts on both engines. It has not yet
been run against a second real host on a switch.

### Not supported yet

- **Unicast VRRP.** Only multicast is supported. It would be needed on a segment that filters multicast between the nodes but
  still passes unicast and ARP. It would cost a per-node configuration: every node's configuration would list its peers, so a
  node joining or leaving would restart the others, or the configuration would use keepalived's `@` tags with a different `-i`
  flag per node. Today one configuration fits every node, and a node joining or leaving restarts nothing. Without multicast
  between the nodes they cannot hear each other and each elects itself, so the address is answered by more than one node.
- **IPv6 virtual addresses.** An IPv6 address is refused with `vip_ipv6_unsupported`: "IPv6 virtual addresses aren't supported yet.
  Use an IPv4 address."
- **An interface override.** See above: each node needs exactly one interface in the address's network.
- Also not in this version: more than one virtual address per environment, per-address node subsets, and a preferred holder or
  weighted priorities.

### The readiness port

The keepalived check asks the node's own proxy whether it is ready, and keepalived runs in the host network namespace, so every
swarm proxy publishes its readiness endpoint on **host port 7483**, in host mode, always, whether or not a virtual IP is on. That
way switching the virtual IP on never rolls the proxies. The endpoint answers a bare status code with no body (200 when the
proxy has applied its bundle and Traefik is up, 503 when not), and it is **reachable on the LAN**: anyone who can reach the node
can ask it and learns only whether that node's proxy is ready. The switch-on's port check (`ingress_port_in_use`) now covers
7483 on a swarm, with 80 and 443.

**The first ensure pass after upgrading rolls every proxy once.** A proxy service made by an earlier version does not publish the
port, so TierPlan updates it to add it, one node at a time, stopping the old task before it starts the new one. Standalone hosts
do not publish the port, because a virtual IP is swarm-only and nothing there would ask for it.

### The keepalived image

The service runs `ghcr.io/ianglennon/tierplan-keepalived`, the fourth TierPlan image, at the same version as TierPlan (`X.Y.Z`,
or `:edge` for a development build) for `linux/amd64` and `linux/arm64`. It holds keepalived 2.4.3, compiled from the unmodified
source tarball from keepalived.org (SHA-256 `a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632`, checked before
the build compiles anything), and TierPlan's own helper, which is the program's PID 1. The image has no shell and no package
manager.

- **Licence.** keepalived is **GPL-2.0-or-later**; the helper and the rest of TierPlan are AGPL-3.0-or-later. The GPL code is in
  this image alone, never in the server, agent or ingress images. The licence text is in the image at
  `/licenses/keepalived/COPYING`, and a notice naming the source and its hash is at `/usr/share/doc/tierplan-keepalived/NOTICE.md`.
  The image labels `org.tierplan.keepalived.source` and `org.tierplan.keepalived.sha256` carry the same.
- **Source.** The unmodified tarball, `keepalived-2.4.3.tar.gz`, is attached to every GitHub release and listed in the signed
  `checksums.txt`, so the source of the binary is available beside the image.
- **Signature and SBOM.** The image is signed with keyless cosign in the release workflow like the other three, and carries an
  SBOM attestation: the helper's Go modules and a hand-written entry for keepalived and OpenSSL. See the
  [release checklist](release-checklist.md) for the commands.
- **What it runs with.** The service drops every capability and adds `NET_ADMIN` and `NET_RAW` for VRRP and ARP, and `SETUID` and
  `SETGID` because the readiness check runs as a script (without them a holder with a dead proxy would never give the address
  up). The root filesystem is read-only with a tmpfs on `/run` and `/tmp`, no Docker socket and no other mount is given, and
  it publishes no port. The key is a swarm secret read as a file, never a value in the service spec.

### Residual risks

- **Anything on the segment can contend for the address.** VRRP authentication only keeps honest neighbours apart; anything on this
  network segment can still contend for the address. A machine on the segment can answer ARP for it, or run a router with
  another router ID or key and answer for the address too. TierPlan reports a foreign router and an address in use. It cannot
  stop either.
- **The opt-in is the whole trade.** A compromised TierPlan could take over every node, as the text above says.
- **Clocks must agree within the 30 second window.** A node whose clock is out of step has its adverts refused, and the nodes
  can then disagree about who holds the address.
- **A split brain is possible when multicast is filtered.** Nodes that cannot hear each other's adverts each hold the address.
  The status then shows `many` holders and the bell raises `vip_multiple_holders`.
- **The readiness port is visible on the LAN**, as above, and tells a scanner only whether a node's proxy is ready.
- **The address check is not proven on a real LAN yet**, as above.

The threat model lists the same risks with their mitigations: see [threat-model.md](threat-model.md#what-phase-26-adds-the-virtual-ip).

## Where the virtual IP will not work

The [virtual IP](#virtual-ip) needs the nodes to share one network segment and to be free to announce an address on it. It
will not work, or will not work reliably, in these places:

- **Cloud networks without layer 2 or multicast.** Most public clouds route traffic between instances and do not let an
  instance claim an extra address with ARP. They also drop multicast, so VRRP adverts to 224.0.0.18 never arrive. Use the
  provider's own floating address or load balancer there.
- **Hypervisors and switches that filter MAC or ARP.** Port security, MAC or ARP filtering and anti-spoofing rules drop the
  virtual address's ARP answers and its gratuitous ARP. On a virtual switch, allow promiscuous mode, MAC address changes and
  forged transmits for the nodes' ports, or turn the filtering off for them.
- **Wi-Fi.** A node on Wi-Fi often cannot announce an address that is not its own, and access points commonly drop multicast
  or ARP between clients. Put the nodes on wired ports.
- **Nodes on different subnets.** VRRP and ARP stay inside one segment, and every node's address must be inside the virtual
  address's prefix. Nodes in different subnets, or behind a router, cannot share the address.

When the nodes cannot hear each other's adverts, each elects itself and the address is answered by more than one node: see
[Not supported yet](#not-supported-yet) and the [residual risks](#residual-risks).

## Stable codes

The codes that TierPlan's own route, the `tierplan-self` network and the HTTP-01 carve-out write, as the code writes them.
The codes of switching ingress itself on and off are in the sections above.

| Code | What it means and what to do |
| --- | --- |
| `self_route_not_here` | Switching TierPlan's own route on is refused: TierPlan runs in another environment, which the sentence names, or it cannot tell which one it runs in (its own Docker socket does not say, or it cannot find its own container). Turn the route on in the environment TierPlan runs in. |
| `self_route_needs_service` | Refused: TierPlan runs as a plain container on a swarm node. The route needs TierPlan to run as a swarm service. Deploy it from the stack file in [install-in-swarm.md](install-in-swarm.md), then try again. |
| `self_route_base_path` | Refused: TierPlan's public URL has a path. The route serves TierPlan at the root of its hostname, so take the path out of the public URL first. |
| `self_route_no_url` | Refused: neither a public URL nor an agent URL is set. Set TierPlan's public URL in Settings first. |
| `self_route_already_on` | Refused: the install already has its own route, in this environment or in another. There is one per install. Turn it off first to serve TierPlan on another hostname. |
| `self_certificate_terms` | Refused: no certificate can be ordered for the hostname, because Let's Encrypt's terms have not been accepted. Accept them under Ingress certificates on the HTTPS page. |
| `self_certificate_paused` | Refused: certificate work is paused after a restore. Resume it under Ingress certificates on the HTTPS page. |
| `self_certificate_pending` | A warning, not a refusal: the hostname has no valid certificate yet. TierPlan orders one when the route is switched on, and the route waits (`waiting_certificate`) until it is ready. |
| `ingress_host_is_agent_url` | A claim, a stack's route or TierPlan's own route, is refused for the host that agents and proxies dial: the agent URL's host, or the public URL's when no agent URL is set. Choose another hostname, or set an agent URL on another host first. |
| `agent_url_is_ingress_host` | Settings refuse (422) an agent URL whose host the ingress serves, TierPlan's own route's or a stack's. Use a hostname that reaches TierPlan directly. |
| `public_url_is_ingress_host` | Settings refuse (422) a public URL whose host the ingress serves while no agent URL that the ingress does not serve is set. Clearing the agent URL under such a public URL is refused with the same code. Set a direct agent URL first, or in the same save. |
| `instance_admin_required` | 403: the self route's previews, switching it on and switching it off need an instance admin as well as `ingress.manage` on the environment. The refusal is audited as `permission.denied`. |
| `self_route_not_on` | 409: the switch-off, or its preview, was asked of an environment where TierPlan's own route is not on. |
| `self_route_check_failed` | The switch-on job could not read what its check needs, so nothing was judged and nothing was written. Try again. |
| `self_route_claim_failed` | The switch-on job could not store the route, for a reason that is not a refusal. Nothing is served. Try again. |
| `self_route_held` | The switch-on job lost the claim to another switch-on, in another environment, that stored its route after this job's check. The job reads `partial`. Turn that route off first if you want it here. |
| `self_route_release_failed` | The route could not be released and is still on. Turn it off again. It also ends the `self_route` step of an ingress switch-off, which then stops; switch off again. |
| `disable_self_network_failed` | The `self_network` step of an ingress switch-off could not read or remove `tierplan-self`, for a reason other than something still being on it. Switch off again. |
| `tierplan_attached` | Not a failure: the `self_network` step of an ingress switch-off kept `tierplan-self`, because TierPlan's own task or container is still on it. See [Folding and removing](#folding-and-removing). |
| `self_not_attached` | TierPlan's own route is on, but TierPlan's swarm service is not on `tierplan-self` yet, so nothing is served on the hostname. It is the route's state, the reason of its condition and a warning in the switch-on preview. Add the stack-file lines (see [Turning it on](#turning-it-on)). |
| `not_observed` | The reason of the route's network condition before an ensure pass has looked at `tierplan-self`. The route's state reads `unknown` until the next pass, within a minute. |
| `self_network_reserved` | A stack's check refuses its compose file: it declares a network called `tierplan-self`, points at it as an external network, or names it as a network mode. Every member of that network can send TierPlan a client address it believes and answer the name the proxies reach TierPlan by, so only TierPlan and the proxies are on it. Use another network. Only the swarm stack TierPlan's own service runs in may name it, as an external network. |
| `self_network_foreign_member` | On the card and in the server log: a service or container other than the ingress proxy and TierPlan is on `tierplan-self`. Until it leaves, TierPlan believes no client address forwarded over the network, so sign-in throttling and the audit log record the proxy's address. Take it off the network. |
| `self_network_foreign` | In the server log: a network called `tierplan-self` exists without both of TierPlan's labels (`tierplan.component=ingress` and `tierplan.ingress.role=self`). TierPlan never attaches, adopts or trusts it. Remove or rename it. |
| `ingress_render_self_input` | The proxies' configuration for TierPlan's own route or the carve-out could not be made from what TierPlan knows of itself (a base path in the public URL, or no address of TierPlan's on the network). The proxies keep the configuration they had, and the server log names the field. |
| `self_network_peer` | An agent's or an ingress proxy's connect reached TierPlan through its own route and was refused. The agent's or the proxy's log names the code. Point the agent at the agent URL, which must not be an ingress hostname. |
| `ingress_http01_conflict` | Switching ingress on is refused: TierPlan runs in this environment, its own certificate uses HTTP-01, and the carve-out cannot reach it. See [TierPlan's own certificate and HTTP-01](#tierplans-own-certificate-and-http-01). |
| `ingress_http01_unknown` | A warning: TierPlan could not tell whether its own certificate needs port 80 here. The switch goes ahead. Check by hand, or use DNS-01. |
| `ingress_http01_carve_out` | A notice in the switch-on preview, not a finding: TierPlan's own certificate uses HTTP-01 and keeps renewing through the carve-out. Nothing needs doing. |
