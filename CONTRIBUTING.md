# Contributing to TierPlan

Thank you for helping. This page covers the rules a change has to follow and how to build and test it. Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md), not through issues or pull requests.

## Licence and sign-off (DCO)

TierPlan is licensed under AGPL-3.0-or-later. By contributing you agree your work can be distributed under it. There is **no CLA**. Instead, every commit carries a `Signed-off-by` line, the [Developer Certificate of Origin](https://developercertificate.org/) sign-off, saying you have the right to submit it.

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
| `make build` | Builds the SPA, then `bin/tierplan` (static, CGO off, SPA embedded). Pass `VERSION=v0.1.0` to stamp a version. |
| `make test` | The Go modules (`make test-go`: race, shuffled, through gotestsum) and then the web unit tests (`make test-web`). |
| `make test-go` | Every Go module with `-race -count=1 -shuffle=on`; the failing tests are listed at the end and the shuffle seed is printed. |
| `make test-web` | The Vitest suite in shuffled order. It prints its seed; `make test-web WEB_SEED=<seed>` reproduces a run. |
| `make test-summary` | Lists the failing and hung tests from the result files in `.devdata/test`. CI runs it after every test job. |
| `make test-stress` | The stability bar (20 shuffled race runs, then `-cpu=1` and `-cpu=4`) for one share of the suite: `STRESS_SHARD=api`, `core`, `rest` or `agent-web`. Takes hours; run by hand, for example before a release or when chasing a flake. |
| `make test-int` | Real-engine Go tests (build tag `integration`) against a Docker daemon. Set `TIERPLAN_TEST_DOCKER_HOST` to use one, otherwise each run starts a `docker:<N>-dind` container (`TIERPLAN_TEST_ENGINE`, default 29). Needs Docker. |
| `make test-swarm` | The multi-node suite (build tag `swarm`): real agents in dind swarms of 3 managers and a worker (`TIERPLAN_TEST_ENGINE`, default 29; CI runs 24 and 29). Builds the agent image first. Needs Docker with buildx. |
| `make dev-swarm`, `make dev-swarm-down` | A local swarm for the development loop, and its removal (see "Local swarm" below). Need Docker with buildx. |
| `make lint` | golangci-lint, Biome and `tsc --noEmit`. `make lint-config` and `make actionlint` check the linter and workflow files. |
| `make gen` | Regenerates `openapi.json`, the TypeScript API types, the sqlc code and `web/public/third-party-notices.json`. Run it whenever you change an API route, a SQL query or a dependency. |
| `make gen-check` | Runs `make gen` and fails if anything changed. CI runs it. The first run needs network access. |
| `make spdx` | Checks SPDX headers (below). |
| `make licences` | Checks dependency licences against the allowlist (below). |
| `make dco` | Checks every commit in a range is signed off. |
| `make e2e` | Playwright against the built image behind Caddy at a subpath, with a `docker:29-dind` sidecar it manages and a second one that hosts a real agent. Needs Docker with buildx and Compose. See below. |
| `make e2e-copy` | The copy button's legacy fallback in a real Chromium on an insecure origin (`http://tierplan.test`), inside a Dialog and an AlertDialog. Needs no Docker. See below. |
| `make proxy-smoke` | The built image behind Caddy, nginx and Traefik with the files in `docs/proxy`, plus three deliberately broken nginx setups. Needs Docker, curl and jq. |
| `make release-check` | Validates `.goreleaser.yaml` and builds this machine's binary the way a release does. |
| `make smoke` | Starts `bin/tierplan` and checks its basic contract. |
| `make image`, `make image-smoke` | Build the release image locally (not pushed) and run checks against it. Need Docker with buildx. |
| `make e2e-down` | Takes the e2e stack down with its volumes. Safe to run when nothing is running. |
| `make ci-clean`, `make ci-preflight` | CI hygiene: `ci-clean` removes the Docker resources labelled `tierplan.ci.owner=$TIERPLAN_CI_OWNER` (required) and nothing else; `ci-preflight` fails when disk space is below `TIERPLAN_CI_MIN_FREE_GB` (default 20). |
| `make hooks`, `make test-scripts` | Install the sign-off hook; prove each policy script fails on a seeded violation. |

CI runs thin wrappers over these same targets, so you can reproduce a CI failure locally with the same command.

### Testing

Tests must be stable, not just green: they run shuffled, tests wait for a signal rather than sleep, polling is limited to conditions outside the test, and a flaky test is fixed or quarantined with a dated entry, never retried. [docs/testing.md](docs/testing.md) has the stability bar, the rules for waiting in tests, the quarantine rules and how to reproduce a failure from its seed. `make lint` enforces the mechanical parts.

### End-to-end tests

`make e2e` builds `tierplan:e2e` and the agent image, generates a throwaway TLS certificate (`scripts/e2e-cert.go`, seven days, under `.devdata/e2e-tls`), and starts TierPlan with Docker Compose behind Caddy at `https://127.0.0.1:8443/tierplan`. Next to it run a `docker:29-dind` daemon that TierPlan manages through a shared unix socket and a second `docker:29-dind` daemon (`agenthost`) that plays a remote host. Playwright drives setup, sign-in, the dashboard, settings and the audit log through the proxy, then the golden path for a remote host: add an environment, run the copied command on `agenthost`, watch it come online, switch to it and revoke it. It always tears the stack down with its volumes. Notes:

- It runs `playwright install --with-deps chromium`, which installs OS libraries and may use `sudo`. On a machine that already has them, run `make e2e E2E_BROWSER_ARGS=chromium`.
- `TIERPLAN_E2E_HTTPS_PORT` moves the published port off 8443. It is the only published port and binds 127.0.0.1.
- `TIERPLAN_E2E_AGENTHOST_IMAGE` runs the remote host on another Engine, for example `make e2e TIERPLAN_E2E_AGENTHOST_IMAGE=docker:24-dind`; the agent spec checks the Engine version on the dashboard against `docker version` on that host.
- Every spec's address carries the `/tierplan` prefix: open pages with `page.goto(at("/audit"))`, not a bare path, because Playwright would drop a base path held in `baseURL`.
- The agent reaches Caddy by its fixed address on the compose network, 172.30.0.10, because a nested Docker cannot resolve compose names, and trusts the harness certificate through `TIERPLAN_AGENT_PIN`, the documented setting for a proxy with its own certificate.
- The HTML report is written to `.devdata/playwright-report`. The agent spec keeps its trace and screenshots even when it passes.
- `make e2e-copy` is a separate, Docker-free suite (`e2e/playwright.copy.config.ts`, specs in `e2e/copy`). It serves a test page, `web/harness/copy`, with Vite and opens it as `http://tierplan.test`, a name Chromium maps to 127.0.0.1. That is an insecure context, where `navigator.clipboard` does not exist, which is how a self-hosted TierPlan is usually opened and the only case in which Copy uses the legacy fallback; jsdom cannot show the bugs there. It uses its own config and does not import `tests/helpers`. `TIERPLAN_COPY_HARNESS_PORT` (default 5179) moves the dev server. CI runs it in the `e2e` job.
- A new spec file must be added to `order` in `e2e/playwright.config.ts` and must import `test` from `./helpers`, which fails the test on any CSP violation or any request to another origin.

## The development loop

```sh
make dev
```

Open <https://localhost:5173/>. Vite serves the SPA there over https with a development certificate and proxies `/api` and `/hooks` to the Go server on `127.0.0.1:7480`, also over https, which air rebuilds when a `.go` file changes. The first `make dev` makes a development CA and a certificate for `localhost`, `127.0.0.1`, `::1` and `dev.homelab.org.uk` under `.devdata/dev-tls`; import `.devdata/dev-tls/ca.pem` in your browser once (`make dev-cert` prints its fingerprint) or click through the warning. The proxy checks the certificate the Go server made for itself (`.devdata/tls/server.crt`) and trusts nothing else, so a Go server with another certificate is refused rather than passed through; `make dev-tls-check` proves that. There is no plain http path in the dev loop and no setting that adds one. Development data lives in `.devdata/` (git-ignored). The Go side is built with the `devspa` tag, so port 7480 serves the SPA from web/dist on disk, as the last make web or pnpm -C web build left it. The Vite dev server never writes web/dist, so edits you see on :5173 are not on :7480 until you build again.

**From another machine.** Make Vite listen beyond loopback and tell both servers the address the browser will use:

```sh
make dev DEV_WEB_HOST=0.0.0.0 DEV_PUBLIC_URL=https://<this machine's name>:5173
```

Then open that address. The public URL's host name is added to the development certificate (more names go in `DEV_CERT_NAMES`), and the same CA signs it again, so a browser that trusts `ca.pem` keeps trusting it. The public URL must be the address in the browser's bar, because the Origin check (D-36) compares the two and refuses every change that comes from any other origin, including the setup-token form. `DEV_PUBLIC_URL=` (empty) leaves the public URL unset, so the setup wizard asks for it. The setup-token gate still applies (D-79). This opens the dev server to your network, so do not do it on a network you do not trust. `DEV_LISTEN` is a different setting: `DEV_LISTEN=0.0.0.0:7480` is what lets the dev swarm's nodes reach the Go server (below), and a remote browser does not need it.

**The setup token flow applies in development too.** A new `.devdata` prints a setup token in the Go server's output, and `TIERPLAN_DATA_DIR=./.devdata go run ./cmd/tierplan setup-token` prints it again. There is deliberately no flag or setting that skips it (D-79), and no change should add one. Development needs a Docker socket to show a host; without one the app shows its "no Docker socket" screens.

### Local swarm

`make dev-swarm` starts four privileged `docker:29-dind` containers (`tierplan-dev-m1` to `tierplan-dev-m3` and `tierplan-dev-w1`) on a network called `tierplan-dev-swarm`, makes them a swarm of three managers and a worker, builds the agent from this tree (`make agent-image`) and loads it on every node under the name the install command uses. It then prints the next steps: start the server with `make dev DEV_LISTEN=0.0.0.0:7480` so the nodes can reach it (to browse from another machine as well, add `DEV_WEB_HOST=0.0.0.0 DEV_PUBLIC_URL=https://<this machine's name>:5173`, see above), add an agent environment in the UI, and paste the swarm command into `docker exec -i tierplan-dev-m1 sh`.

`make dev-swarm-down` removes those four containers, their volumes and the network, by name, and only if they carry the label `tierplan.dev-swarm=true`. It never runs a prune command, so nothing else on your Docker host is touched. `DEV_SWARM_ENGINE` (default 29) and `DEV_SWARM_SUBNET` (default `172.31.250.0/24`) change the engine version and the network.

`DEV_SWARM_DNS` sets the DNS servers the nodes' Docker daemons hand to the agents, as IP addresses separated by commas or spaces. Without it the script uses the host's non-loopback IPv4 nameservers from `/etc/resolv.conf`, or from `/run/systemd/resolve/resolv.conf` when the first file lists only loopback ones such as systemd-resolved's `127.0.0.53`. This matters because the nodes sit on a user-defined network, where each node sees only Docker's own resolver at `127.0.0.11`; the inner daemon strips that as loopback and, with nothing left, falls back to `8.8.8.8`, which cannot resolve your LAN names. Run `scripts/dev-swarm.sh dns` to see what would be used; with none found it says so and how to fix it. A value that is not a non-loopback IP address is refused before anything is created.

Things to know:

- The agents dial the server over `wss://` and verify it. `make dev-go` sets `TIERPLAN_AGENT_URL=https://<gateway>:7480` and leaves the server in its default mode, where it makes its own self-signed certificate, so the install command TierPlan shows carries that certificate's pin in `TIERPLAN_AGENT_PIN`. Because the session is TLS-verified, "Update agent" and "Also uninstall the agent from the hosts" work end to end on the dev swarm. `DEV_LISTEN=0.0.0.0:7480` also makes the Go server reachable from your network; the setup-token gate still applies, but do not do this on a network you do not trust.
- Agents installed in an existing dev swarm with the old command for a plain connection must be reinstalled with the new `wss`-based command from Add environment: they will not connect to a server that only speaks TLS.
- If your shell predates your `docker` group membership, run the targets through `sg docker -c 'make dev-swarm'`. Both `make test-swarm` and `make dev-swarm` need Docker to run privileged containers, which is root-equivalent on that machine.

## SPDX headers

Every tracked `.go`, `.ts`, `.tsx` and `.sql` file starts with these two lines (in that file's comment syntax), within its first five lines:

```
SPDX-License-Identifier: AGPL-3.0-or-later
SPDX-FileCopyrightText: 2026 The TierPlan Authors
```

`make spdx` fails on a file without them. Only these are exempt: generated files (`Code generated ... DO NOT EDIT.`, `web/src/api/schema.d.ts`, `web/src/routeTree.gen.ts`, `internal/store/sqlc/`) and the deliberately header-less fail-first fixture `scripts/testdata/spdx/missing.go`. Nothing else is exempt, including copied-in third-party code.

Components copied in with `shadcn add` (into `web/src/components/ui/`) must get these three lines at the top before you commit, next to the step that rewrites their `cn` import to `@/lib/utils`:

```
// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
// SPDX-FileCopyrightText: 2023 shadcn. Derived from shadcn/ui (https://github.com/shadcn-ui/ui), used under the MIT licence; see the third-party notices.
```

A leading comment before a `"use client"` directive is fine. The third line keeps the MIT attribution; the MIT notice for shadcn/ui is also listed in the third-party notices (`make gen` regenerates `web/public/third-party-notices.json`, which Settings > About shows).

## Dependency licences

TierPlan is AGPL, so what it ships with must be compatible. `make licences` checks the Go dependencies of the server binary (for `linux/amd64` and `linux/arm64`) and the production pnpm dependencies against an allowlist in `scripts/licence-check.mjs`: MIT, BSD-2-Clause, BSD-3-Clause, ISC, Apache-2.0, MPL-2.0, 0BSD, Unlicense, CC0-1.0, Zlib, LGPL-3.0-or-later and GPL-3.0-or-later. Anything else fails, including unknown licences. GPL-2.0-only, SSPL, BUSL and Commons Clause code must not be added at all.

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
