<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Coaming Authors -->

# Registry credentials

Coaming stores the credentials for the registries your images come from, once, and uses
them to pull images and to browse tags. This page explains what to put in a credential, how
Coaming chooses one for a pull, and what it does and does not do to your hosts.

## Adding a credential

Add credentials under **Registries**. Pick the registry type and give it a username and a
secret. The secret is encrypted when you save it and cannot be shown again.

| Type | Registry | What to use as the secret |
|------|----------|---------------------------|
| Docker Hub | `docker.io` | A Docker Hub **access token**, not your password. Create one under Account settings, Personal access tokens. |
| GHCR | `ghcr.io` | A **personal access token (classic)** with the `read:packages` scope. |
| GitLab | `registry.gitlab.com` (editable) | A **deploy token** with `read_registry`, or a personal access token with `read_registry`. |
| Generic v2 | The registry you name | A static token or password. Amazon ECR tokens expire after 12 hours, so they need replacing often. |

Use **least-privilege, read-only tokens**. Every node that pulls an image receives the
credential for that pull (see below), so give it no more than pulling needs.

## Two layers

A credential is either an **instance** credential, which applies to every environment, or an
**environment** credential, which applies to one environment only. Instance credentials
need an instance admin. An environment credential never reaches another environment.

## How a credential is chosen

When you pull an image, Coaming picks the most specific matching credential. There is no
list of who may use which credential: anyone allowed to pull in the environment uses the
match.

1. The credential's registry must be the image's registry exactly. `ghcr.io` never matches
   `ghcr.io.evil.example`.
2. A credential may have a namespace prefix, such as `acme` or `acme/team`. It matches
   images below that path only, on whole path segments: `acme` covers `acme/app` and never
   `acmecorp/app`. With no prefix it covers the whole registry.
3. The **longest prefix wins**, across both layers.
4. When two credentials are equally specific, the **environment's wins** over the
   instance's.
5. For Docker Hub only, a credential can be ticked **Use for all Docker Hub pulls**. It is
   used last, for Hub images no more specific credential covers. Each layer has one.

The **Which credential?** card on the Registries page and the line under the image field in
the Pull dialog show the choice before you pull, and why.

## Pulling on every node

A pull picks the credential once and sends it with the request to each node. The pull
progress panel names it (`Credential: “ghcr-acme” (Instance)`, or `Credential: anonymous`),
and so does the `image.pulled` audit row of each node.

- On a swarm, every node you pull on receives the credential and pulls with it. A node that
  has never logged in works.
- Node pulls follow each node's own Docker daemon settings, including its registry mirrors.
  Coaming does not change them.
- If the registry refuses the pull, Coaming says which credential was used and offers to
  test it, or says no credential matched and offers to add one. It does **not** retry and
  does **not** try another credential. A refusal is also remembered on the credential
  (**Last use**) so you can see it.

## Swarm services

When Coaming creates or updates a swarm service, it sends the matching credential with that
request, and the engine keeps it with the service so that any node, including one that has
never logged in, can pull the image. The credential is not shown when you inspect the
service.

A service keeps the credential it was **last written with**. If you rotate a token in
Coaming, a service that is already running keeps pulling with the old one until it is next
updated through Coaming.

## What Coaming never does to your hosts

- It never runs `docker login` on a host.
- It never writes a credential into a daemon configuration or `~/.docker/config.json`.
- It never reads either of them. A credential is what you stored in Coaming and nothing
  else.

Credentials travel only with the request that needs them, over the connection to the
node's agent, and are not written to logs, audit rows, job state or API responses.

## Self-hosted registries

A registry you run yourself, such as `registry:2`, Harbor or a GitLab registry on your own
network, often has a certificate from your own certificate authority, or serves plain HTTP.
Add it as a **Generic v2** credential (or **GitLab** with your own host), then open
**Advanced** in the credential sheet.

- **Custom CA certificate (PEM)**: paste the certificate authority that signed the
  registry's certificate. Coaming trusts it for this credential's calls only, in addition
  to the system's own authorities. It is at most 64 KiB and must hold a certificate.
- **Allow plain HTTP or skip TLS verification**: Coaming first tries HTTPS without checking
  the certificate and, only when the server answers plain HTTP, uses plain HTTP. This is the
  way Docker's `insecure-registries` behaves. It is off by default, and the sheet warns
  while it is on: anyone on the network path could read the credential. Use a CA instead
  whenever you can.

These two settings affect **only Coaming's own calls**: the credential test and tag
browsing. They do not change how a node pulls.

### Each node needs its own setup

A node pulls with its own Docker daemon, which does not know about Coaming's settings. To
pull from a registry with its own CA or without HTTPS, set up each node that will pull:

- **A CA**: put the CA certificate at `/etc/docker/certs.d/<host:port>/ca.crt` on the node
  (for example `/etc/docker/certs.d/registry.lan:5000/ca.crt`). No restart is needed.
- **Plain HTTP or an untrusted certificate**: add the registry to `insecure-registries` in
  the node's `/etc/docker/daemon.json` and restart Docker.

Coaming never edits daemon settings, never writes to `/etc/docker` and never runs
`docker login`. When a node refuses a pull for a certificate or HTTPS reason, the pull
panel says so and points here.

### Registry mirrors are ignored

Coaming's own calls always go to the registry named in the credential. They ignore the
registry mirrors set on your nodes (`registry-mirrors` in `daemon.json`) and any mirror
setting of a Docker client. Node pulls still follow each node's own mirrors. To browse or
test a mirror from Coaming, add the mirror as its own **Generic v2** credential.

### Limiting where the server connects

The Coaming server always refuses link-local (cloud metadata), unspecified and multicast
addresses for registry calls, including on the plain HTTP fallback. It allows private and
loopback addresses, because self-hosted registries live there. To refuse more, set
`COAMING_REGISTRY_BLOCK_CIDRS` to a comma-separated list of IPs or CIDRs, such as
`10.9.0.0/16,fd00::/8`. An entry that is not an IP address or CIDR stops Coaming at startup
and names the entry.
