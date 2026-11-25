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
- [Virtual IP](#virtual-ip)

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
| `ingress_name_shadowed` | Not a preview finding: the switch-on job checks it once it has made the ingress network, because only then is the network's ID known. A container on the host, in any state, is joined to a network by a name that is the start of that ID (such a name is made only of the digits 0-9 and the letters a-f). Docker reads a name it cannot find as the start of a network's ID, so after a reboot that container could join the ingress network under a name of its own. Ingress stays off and the job names the container. Disconnect it from that network or remove it and switch on again, or switch off and on again, which makes the network with a new ID. While ingress is on, every pass of the ensure loop runs the same check, and again after anything it brings up: on a hit the proxy is taken down and kept down while that network is there, and the status reports `name_shadowed` in `lastEnsure`; the server log names the container and the network name it is stored on. Once the container is disconnected or removed, the next pass brings the proxy back up. |
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

## Virtual IP

A virtual IP gives a swarm's ingress one floating IPv4 address. keepalived runs on every node as a global service, the nodes
elect one holder with VRRP, and the holder answers for the address. You point DNS or your router at that one address, and it
keeps working when a node goes away. TierPlan is not in the path: if TierPlan is down, the address still moves. A node holds
the address only while its own proxy is ready, so a node whose proxy has stopped, or has no routes yet, does not take it.

The virtual IP is for swarms. A standalone host has one node and its own address already reaches its proxy, so the switch-on
is refused there with `vip_swarm_only`. Like the rest of this page, it is an API call in this version. Every route below is
under `/api/env/{envId}/ingress/vip` and needs the `ingress.manage` permission on the environment.

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

Two things in that history are normal and may surprise you:

- **A switch-off usually stores no `disabled` move.** Removing the service stops keepalived on every node at about the same
  moment, so the other nodes are usually stopped before they could report taking the address. Any move stored while a switch-off
  runs has the reason `disabled`, but do not expect one.
- **During an election the address can pass through more than one node.** Every node has the same priority, so when a holder
  goes away all the backups can claim the address in the same second, and the one with the highest address keeps it. Each step is
  one move, and the later steps have the reason `unknown`. A first election can store one too, so a new virtual IP can show
  one move in the bell. The address and the history are correct; wait for the holder to settle before you read it.

Bell items, for a caller who manages the environment's ingress: `vip_no_holder` (an error: no node has held the address for more
than ten seconds, except while a node's keepalived has run for under fifteen seconds, which is a normal first start),
`vip_multiple_holders`, `vip_moved` (one item per environment however many moves there were in the last hour, with the count; a
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
