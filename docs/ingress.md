<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# Ingress

Ingress lets a stack's compose file ask for a public hostname, and TierPlan runs a Traefik proxy that serves that
hostname over HTTPS: one on every node of a swarm, and one on a standalone host (a host with an agent, or TierPlan's own
local socket). This page covers the part you operate in this version: switching ingress on and off for an environment,
what that creates and what stops it, how existing stacks join, and what the proxy's access log records. The sections up
to [On a standalone host](#on-a-standalone-host) describe a swarm; that section says what differs on a standalone host. Every step on this page is an API call in this version. The page in the web interface
comes in a later version, and until then you use the calls below with a token that has the `ingress.manage` permission
on the environment (an Admin has it; see [permissions.md](permissions.md)).

- [What switching on creates](#what-switching-on-creates)
- [Switching on](#switching-on)
- [What refuses the switch](#what-refuses-the-switch)
- [On a standalone host](#on-a-standalone-host)
- [TierPlan's own certificate and HTTP-01](#tierplans-own-certificate-and-http-01)
- [Existing stacks need a redeploy](#existing-stacks-need-a-redeploy)
- [The status](#the-status)
- [The join token](#the-join-token)
- [The access log](#the-access-log)
- [Switching off](#switching-off)
- [Nodes that were away](#nodes-that-were-away)

## What switching on creates

Switching on creates four things in the swarm, all labelled `tierplan.component=ingress`, and none of them is yours to
edit: TierPlan shows them read-only and refuses to remove or change them while ingress is on.

| What | Name | Notes |
| --- | --- | --- |
| Join token secret | `tierplan-ingress-token-g<generation>-<environment>-<token>` | A swarm secret that holds the token each proxy enrols with. The token is not shown anywhere. |
| Overlay network | `tierplan-ingress` | A swarm overlay on a free /22 block of the swarm's address pools (or the subnet you give). It is not attachable, so a plain `docker run --network tierplan-ingress` is refused. That is not a hard barrier; do not rely on it for isolation. |
| Proxy service | `tierplan-ingress` | A global service, so one task runs on every node. It runs as user 1000, with no Docker socket and no extra capability. It publishes ports 80 and 443 of every node in host mode, so the proxy sees each client's own address. |
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
| `ingress_port_in_use` | A swarm service, in a stack or not, publishes TCP port 80 or 443 (in ingress mode or host mode), or a container on a node does. The finding lists each one. A UDP 443 publisher (HTTP/3) does not block. |
| `ingress_no_address_space` | The swarm's address pools have no free /22 block. |
| `ingress_subnet_invalid` | The subnet you gave is not an IPv4 CIDR block. |
| `ingress_subnet_overlaps` | The subnet you gave overlaps an existing network. The finding lists them. |
| `ingress_name_taken` | The swarm already holds a service, a network or a join token secret under a name ingress uses that is not TierPlan's. Rename or remove it. The check does not look at volumes: the certificate cache volume is a per node object, and a volume of that name that does not carry TierPlan's label would be mounted by the proxy, and switching off leaves it in place. |
| `ingress_url_unset` | TierPlan does not know its own address, which the proxies dial. Set the agent URL or the public URL. |
| `ingress_url_routed` | TierPlan's own address is a hostname that a route or a reservation holds, so the proxies would reach TierPlan through themselves. |
| `ingress_http01_conflict` | See the next section. |
| `ingress_paused` | A restore paused ingress on this environment. An Admin resumes it first with `POST .../ingress/resume`. |

Two findings are warnings and never refuse. `ingress_nodes_not_checked` lists the nodes whose ports could not be read
(a node that is down does not block the switch). `ingress_http01_unknown` says TierPlan could not tell whether the
next section applies.

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
| `ingress_name_shadowed` | Not a preview finding: the switch-on job checks it once it has made the ingress network, because only then is the network's ID known. A container on the host, in any state, is joined to a network by a name that is the start of that ID (such a name is made only of the digits 0-9 and the letters a-f). Docker reads a name it cannot find as the start of a network's ID, so after a reboot that container could join the ingress network under a name of its own. Ingress stays off and the job names the container. Disconnect it from that network or remove it and switch on again, or switch off and on again, which makes the network with a new ID. If the ingress network has to be made again later, the same check runs: the proxy is taken down, the status reports `name_shadowed` in `lastEnsure`, and the next pass makes the network again with a new ID. |
| `ingress_http01_conflict` | TierPlan's own certificate uses HTTP-01 and the host is TierPlan's own host. This includes the local socket environment, and an agent environment whose engine is the one TierPlan itself talks to. Switch TierPlan's own certificate to DNS-01 first (see [https.md](https.md)). A later version lifts this limit. |

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

**The local socket.** TierPlan's own host works with no agent, and the proxy dials TierPlan from inside a container.
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

The proxies take port 80 on every node. If TierPlan itself runs on a node of the swarm and its own certificate is
ordered with the HTTP-01 challenge, which also needs port 80, the next renewal would fail weeks later without any
warning. So in this version the switch is refused with `ingress_http01_conflict`: switch TierPlan's own certificate to
DNS-01 first (see [https.md](https.md)), then switch ingress on. A later version lifts this limit.
When TierPlan runs outside the swarm, nothing conflicts and nothing is shown.

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
