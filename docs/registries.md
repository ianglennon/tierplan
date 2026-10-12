<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Bayplan Authors -->

# Registry credentials

Bayplan stores the credentials for the registries your images come from, once, and uses
them to pull images and to browse tags. This page explains what to put in a credential, how
Bayplan chooses one for a pull, and what it does and does not do to your hosts.

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

When you pull an image, Bayplan picks the most specific matching credential. There is no
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
  Bayplan does not change them.
- If the registry refuses the pull, Bayplan says which credential was used and offers to
  test it, or says no credential matched and offers to add one. It does **not** retry and
  does **not** try another credential. A refusal is also remembered on the credential
  (**Last use**) so you can see it.

## Swarm services

When Bayplan creates or updates a swarm service, it sends the matching credential with that
request, and the engine keeps it with the service so that any node, including one that has
never logged in, can pull the image. The credential is not shown when you inspect the
service.

A service keeps the credential it was **last written with**. If you rotate a token in
Bayplan, a service that is already running keeps pulling with the old one until it is next
updated through Bayplan.

## What Bayplan never does to your hosts

- It never runs `docker login` on a host.
- It never writes a credential into a daemon configuration or `~/.docker/config.json`.
- It never reads either of them. A credential is what you stored in Bayplan and nothing
  else.

Credentials travel only with the request that needs them, over the connection to the
node's agent, and are not written to logs, audit rows, job state or API responses.
