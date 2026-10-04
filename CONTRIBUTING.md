# Contributing to Bayplan

Thank you for helping. This page covers the rules a change has to follow and how to build and test it. Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md), not through issues or pull requests.

## Licence and sign-off (DCO)

Bayplan is licensed under AGPL-3.0-or-later. By contributing you agree your work can be distributed under it. There is **no CLA**. Instead, every commit carries a `Signed-off-by` line, the [Developer Certificate of Origin](https://developercertificate.org/) sign-off, saying you have the right to submit it.

Add it with `git commit -s`, or install the hook once and forget about it:

```sh
make hooks     # sets core.hooksPath to scripts/git-hooks (a prepare-commit-msg hook)
```

The hook appends `Signed-off-by: Your Name <you@example.com>` from `git config user.name` and `user.email`, so set those first. CI runs `make dco` on every pull request and fails on a commit without the line. Locally `make dco` compares against `origin/main` (override with `DCO_BASE=<ref>`), so it fails on a clone that has no `origin/main`.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary`, with a type such as `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `ci` or `chore`. A `!` after the type marks a breaking change. Release notes are grouped by this type, so the first line should read well to a user.

## Prerequisites

- **Go 1.27** (`go.mod` says `go 1.27`).
- **Node 24** (the version CI uses) and **pnpm through corepack**. The version is pinned in `web/package.json` (`packageManager`), so run `corepack enable` once and use `pnpm` normally. Do not install pnpm with `npm i -g pnpm`: the pinned version would be ignored.
- **Docker is optional.** You need it only for `make test-int`, `make test-swarm`, `make dev-swarm` and `make e2e`, plus `make image` and `make image-smoke`. Membership of the `docker` group is root-equivalent on that machine; use a throwaway machine or VM if that worries you.

Known problem: pnpm 12 does not start under **corepack 0.24**. That corepack looks for a `pnpm.cjs` entry point and pnpm 12 ships `.mjs` files, so `pnpm` fails to launch. Use a newer corepack, or a pnpm 12 you installed another way. The author's host worked around it with local shims inside corepack's cache, which is a workaround, not something the repository carries. Check `corepack --version` if `pnpm` fails straight after `corepack enable`.

An optional **dev container** (`.devcontainer/devcontainer.json`) provides Go 1.27, Node 24, pnpm through corepack and Docker-in-Docker, runs `make hooks` after it is created, and forwards ports 5173 and 7480. Its Docker daemon is separate from your host's. Running `make test-int` and `make e2e` inside it has not been tried.

No tool is added to `go.mod` for development: golangci-lint, sqlc, goreleaser, air and actionlint all run through `go run` at the versions pinned in the Makefile, so the first run of each compiles it and takes a while.

## Make targets

| Target | What it does |
|--------|--------------|
| `make dev` | The development loop: Go with live reload plus Vite with hot reload (see below). |
| `make build` | Builds the SPA, then `bin/bayplan` (static, CGO off, SPA embedded). Pass `VERSION=v0.1.0` to stamp a version. |
| `make test` | `go test ./... -race` and the web unit tests (Vitest). |
| `make test-int` | Real-engine Go tests (build tag `integration`) against a Docker daemon. Set `BAYPLAN_TEST_DOCKER_HOST` to use one, otherwise each run starts a `docker:<N>-dind` container (`BAYPLAN_TEST_ENGINE`, default 29). Needs Docker. |
| `make test-swarm` | The multi-node suite (build tag `swarm`): real agents in dind swarms of 3 managers and a worker (`BAYPLAN_TEST_ENGINE`, default 29; CI runs 24 and 29). Builds the agent image first. Needs Docker with buildx. |
| `make dev-swarm`, `make dev-swarm-down` | A local swarm for the development loop, and its removal (see "Local swarm" below). Need Docker with buildx. |
| `make lint` | golangci-lint, Biome and `tsc --noEmit`. `make lint-config` and `make actionlint` check the linter and workflow files. |
| `make gen` | Regenerates `openapi.json`, the TypeScript API types, the sqlc code and `web/public/third-party-notices.json`. Run it whenever you change an API route, a SQL query or a dependency. |
| `make gen-check` | Runs `make gen` and fails if anything changed. CI runs it. The first run needs network access. |
| `make spdx` | Checks SPDX headers (below). |
| `make licences` | Checks dependency licences against the allowlist (below). |
| `make dco` | Checks every commit in a range is signed off. |
| `make e2e` | Playwright against the built image, with a `docker:29-dind` sidecar. Needs Docker with buildx and Compose. See below. |
| `make release-check` | Validates `.goreleaser.yaml` and builds this machine's binary the way a release does. |
| `make smoke` | Starts `bin/bayplan` and checks its basic contract. |
| `make image`, `make image-smoke` | Build the release image locally (not pushed) and run checks against it. Need Docker with buildx. |
| `make hooks`, `make test-scripts` | Install the sign-off hook; prove each policy script fails on a seeded violation. |

CI runs thin wrappers over these same targets, so you can reproduce a CI failure locally with the same command.

### End-to-end tests

`make e2e` builds `bayplan:e2e` from the current tree, starts it with Docker Compose next to a `docker:29-dind` daemon that it manages through a shared unix socket, and drives setup, sign-in, the dashboard, settings and the audit log in Chromium. It always tears the stack down with its volumes. Notes:

- It runs `playwright install --with-deps chromium`, which installs OS libraries and may use `sudo`. On a machine that already has them, run `make e2e E2E_BROWSER_ARGS=chromium`.
- `BAYPLAN_E2E_PORT` moves the published port off 7480.
- The HTML report is written to `.devdata/playwright-report`.
- A new spec file must be added to `order` in `e2e/playwright.config.ts` and must import `test` from `./helpers`, which fails the test on any CSP violation or any request to another origin.

## The development loop

```sh
make dev
```

Open <http://localhost:5173/>. Vite serves the SPA there and proxies `/api` to the Go server on `127.0.0.1:7480`, which air rebuilds when a `.go` file changes. Development data lives in `.devdata/` (git-ignored). The Go side is built with the `devspa` tag, so it serves the SPA from `web/dist` on disk.

**The setup token flow applies in development too.** A new `.devdata` prints a setup token in the Go server's output, and `BAYPLAN_DATA_DIR=./.devdata go run ./cmd/bayplan setup-token` prints it again. There is deliberately no flag or setting that skips it (D-79), and no change should add one. Development needs a Docker socket to show a host; without one the app shows its "no Docker socket" screens.

### Local swarm

`make dev-swarm` starts four privileged `docker:29-dind` containers (`bayplan-dev-m1` to `bayplan-dev-m3` and `bayplan-dev-w1`) on a network called `bayplan-dev-swarm`, makes them a swarm of three managers and a worker, builds the agent from this tree (`make agent-image`) and loads it on every node under the name the install command uses. It then prints the next steps: start the server with `make dev DEV_LISTEN=0.0.0.0:7480` so the nodes can reach it, add an agent environment in the UI, and paste the swarm command into `docker exec -i bayplan-dev-m1 sh`.

`make dev-swarm-down` removes those four containers, their volumes and the network, by name, and only if they carry the label `bayplan.dev-swarm=true`. It never runs a prune command, so nothing else on your Docker host is touched. `DEV_SWARM_ENGINE` (default 29) and `DEV_SWARM_SUBNET` (default `172.31.250.0/24`) change the engine version and the network.

Three things to know:

- Remote update and uninstall need a TLS-verified session. An agent refuses an `update` or an `uninstall` from the server over a session that is not TLS-verified, which is what the plain `ws://` connection of this swarm is, and answers `insecure_session`. So on the dev swarm "Update agent" shows "Update failed: the agent's connection isn't TLS-verified, so it won't update itself", and removing an environment with "Also uninstall the agent from the hosts" falls back to the manual cleanup command. To exercise either end to end, use a server with a certificate the agents verify (for the real-engine tests, `servertest` does this with its self-signed certificate and the pins in the install command).
- The agents dial the server over plain `ws://`, because there is no certificate for a bare address. `make dev-go` sets `BAYPLAN_AGENT_URL` and `BAYPLAN_AGENT_ALLOW_INSECURE=true` for this, and the server logs a warning at start while the second is on. That is for development only and is set nowhere else. `DEV_LISTEN=0.0.0.0` also makes the dev server reachable from your network; the setup-token gate still applies, but do not do this on a network you do not trust.
- If your shell predates your `docker` group membership, run the targets through `sg docker -c 'make dev-swarm'`. Both `make test-swarm` and `make dev-swarm` need Docker to run privileged containers, which is root-equivalent on that machine.

## SPDX headers

Every tracked `.go`, `.ts`, `.tsx` and `.sql` file starts with these two lines (in that file's comment syntax), within its first five lines:

```
SPDX-License-Identifier: AGPL-3.0-or-later
SPDX-FileCopyrightText: 2026 The Bayplan Authors
```

`make spdx` fails on a file without them. Only these are exempt: generated files (`Code generated ... DO NOT EDIT.`, `web/src/api/schema.d.ts`, `web/src/routeTree.gen.ts`, `internal/store/sqlc/`) and the deliberately header-less fail-first fixture `scripts/testdata/spdx/missing.go`. Nothing else is exempt, including copied-in third-party code.

Components copied in with `shadcn add` (into `web/src/components/ui/`) must get these three lines at the top before you commit, next to the step that rewrites their `cn` import to `@/lib/utils`:

```
// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
// SPDX-FileCopyrightText: 2023 shadcn. Derived from shadcn/ui (https://github.com/shadcn-ui/ui), used under the MIT licence; see the third-party notices.
```

A leading comment before a `"use client"` directive is fine. The third line keeps the MIT attribution; the MIT notice for shadcn/ui is also listed in the third-party notices (`make gen` regenerates `web/public/third-party-notices.json`, which Settings > About shows).

## Dependency licences

Bayplan is AGPL, so what it ships with must be compatible. `make licences` checks the Go dependencies of the server binary (for `linux/amd64` and `linux/arm64`) and the production pnpm dependencies against an allowlist in `scripts/licence-check.mjs`: MIT, BSD-2-Clause, BSD-3-Clause, ISC, Apache-2.0, MPL-2.0, 0BSD, Unlicense, CC0-1.0, Zlib, LGPL-3.0-or-later and GPL-3.0-or-later. Anything else fails, including unknown licences. GPL-2.0-only, SSPL, BUSL and Commons Clause code must not be added at all.

A package under another licence needs a reviewed line in `licence-exceptions.txt` (`<go|npm> <package> <licence>  # reason`) that says why it is acceptable. An exception is a decision, not a convenience, and it matches the exact licence, so a later change of licence fails again.

## New dependencies

Adding a package is a review decision. Say in the pull request why it is needed. pnpm refuses packages published less than 24 hours ago (`minimumReleaseAge`), installs in CI are frozen to the lockfile, and every GitHub Action is pinned to a full commit SHA. Do not add a package whose licence is not on the allowlist, and do not substitute a similarly named package when one fails to install.

## Architecture rules the linter enforces

- Docker is reached only through `internal/dockerx`, which exports typed, read-only methods. The Docker client is not imported anywhere else.
- Routes are registered through `httpx.Register` (or `Router.Guarded`/`Exempt`), never directly with huma, and every one carries a permission requirement.
- `client.FromEnv` and chi's `middleware.RealIP` are banned: the first honours `tcp://` hosts and an environment-chosen API version, the second lets a client forge the address in the audit log. Client addresses come from the trusted-proxy logic (`httpx.ClientIP`).
- `github.com/docker/docker`, `github.com/gorilla/websocket` and `github.com/mattn/go-sqlite3` may not be imported; the lint messages name the replacement.

## Planning files stay local

`.planning/`, `.claude/`, `CLAUDE.md` and `.gsd/` belong to the maintainer's workflow. They are git-ignored, are never committed or pushed, and must not be added with `git add -f`. Stage files by explicit path, not `git add -A`.
