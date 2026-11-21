<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# Ingress on a swarm

Ingress lets a stack's compose file ask for a public hostname, and TierPlan runs a Traefik proxy on every node of the
swarm that serves that hostname over HTTPS. This page covers the part you operate in this version: switching ingress
on and off for a swarm environment, what that creates and what stops it, how existing stacks join, and what the
proxy's access log records. Every step on this page is an API call in this version. The page in the web interface
comes in a later version, and until then you use the calls below with a token that has the `ingress.manage` permission
on the environment (an Admin has it; see [permissions.md](permissions.md)).

Ingress is for swarm environments. A standalone host answers `ingress_standalone_unavailable` (409) and is not
supported yet.

- [What switching on creates](#what-switching-on-creates)
- [Switching on](#switching-on)
- [What refuses the switch](#what-refuses-the-switch)
- [TierPlan's own certificate and HTTP-01](#tierplans-own-certificate-and-http-01)
- [Existing stacks need a redeploy](#existing-stacks-need-a-redeploy)
- [The status](#the-status)
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

If a step of the job fails, the steps after it are not run and nothing is rolled back. Switching on again starts from
what is there. Only one operation runs on an environment at a time: while a stack deploy or another ingress job holds the
environment, the call answers `operation_locked` (409) and writes nothing.

## What refuses the switch

A refusal is a `409` with the problem `ingress_preflight_refused`, and each finding in its `errors` carries a stable
code. The preview shows the same findings, so you can see them before you switch. Nothing has been written when one
appears.

| Code | What is in the way |
| --- | --- |
| `ingress_standalone_unavailable` | The environment is a standalone host. |
| `ingress_no_manager` | No manager node's agent is connected, so TierPlan cannot write to the swarm. |
| `ingress_port_in_use` | A swarm service, in a stack or not, publishes TCP port 80 or 443 (in ingress mode or host mode), or a container on a node does. The finding lists each one. A UDP 443 publisher (HTTP/3) does not block. |
| `ingress_no_address_space` | The swarm's address pools have no free /22 block. |
| `ingress_subnet_invalid` | The subnet you gave is not an IPv4 CIDR block. |
| `ingress_subnet_overlaps` | The subnet you gave overlaps an existing network. The finding lists them. |
| `ingress_name_taken` | The swarm already holds a network, service, secret or volume under a name ingress uses that is not TierPlan's. Rename or remove it. |
| `ingress_url_unset` | TierPlan does not know its own address, which the proxies dial. Set the agent URL or the public URL. |
| `ingress_url_routed` | TierPlan's own address is a hostname that a route or a reservation holds, so the proxies would reach TierPlan through themselves. |
| `ingress_http01_conflict` | See the next section. |
| `ingress_paused` | A restore paused ingress on this environment. An Admin resumes it first with `POST .../ingress/resume`. |

Two findings are warnings and never refuse. `ingress_nodes_not_checked` lists the nodes whose ports could not be read
(a node that is down does not block the switch). `ingress_http01_unknown` says TierPlan could not tell whether the
next section applies.

## TierPlan's own certificate and HTTP-01

The proxies take port 80 on every node. If TierPlan itself runs on a node of the swarm and its own certificate is
ordered with the HTTP-01 challenge, which also needs port 80, the next renewal would fail weeks later without any
warning. So in this version the switch is refused with `ingress_http01_conflict`: switch TierPlan's own certificate to
DNS-01 first (see [https.md](https.md)), then switch ingress on. A later version lifts this limit.
When TierPlan runs outside the swarm, nothing conflicts and nothing is shown.

## Existing stacks need a redeploy

Switching ingress on changes no stack. A service joins the `tierplan-ingress` network when the stack is deployed with
ingress on, and only a service that declares a route joins; the rest of the stack stays off the network. So a stack that
already asked for a route before you switched ingress on has to be redeployed once.

The preview and the status list those stacks (`ingress_redeploy_needed`, a warning), and until a stack is redeployed
its routes read "not attached" on its page. The redeploy is yours to choose, because it replaces the stack's tasks.

## The status

`GET /api/env/{envId}/ingress/status` says whether ingress is on and paused, whether the access log is on, which of the
network, the service and the secret the swarm holds, and how many proxies are expected (one per node), enrolled and
ready (enrolled, healthy and running the current configuration). It also reports the result of the last pass that checks
the objects, the number of certificate cache volumes still waiting to be wiped (see below) and the stacks to redeploy.

TierPlan checks the objects once a minute and whenever the ingress state changes, and puts right what differs: a
missing secret, network or service, or a service that runs a different image than TierPlan's version. It writes only
when something differs, and never while another operation holds the environment.

## The access log

The proxy writes no access log until you switch it on. It is off by default.

    PUT /api/env/{envId}/ingress/access-logs     {"enabled": true}

The answer says when it applies: `rolling` (the proxy service is updated, one node at a time, so each proxy restarts in
turn), `when_enabled` (stored now, applies when ingress is switched on) or `when_resumed` (stored now, applies when an
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
3. removes the certificate cache volume on each node through that node's agent;
4. removes the network and the join token secrets;
5. disables the join token and removes the proxy nodes from the list.

A step that fails stops the job, and you switch off again to carry on. Certificates are not touched: they belong to the
hostnames, not to the proxies.

## Nodes that were away

The cache volume holds the private key of every certificate the proxy served, so switching off must not leave it behind
on a node that was down. A node whose agent is not connected when you switch off is named in the job and its volume
is kept as a **pending wipe**. The volume is removed as soon as that node's agent connects again, and the entry is
cleared and audited (`ingress.wipe_queued`, `ingress.wipe_done`). The status counts the pending wipes. Switching off
again retries the ones that remain.

A node that still runs a proxy container is never wiped. If you switch ingress on again before a pending wipe ran,
the entry is dropped without wiping, because the proxy owns the volume again. A node that left the swarm for good
keeps its entry, since there is no agent to reconnect.
