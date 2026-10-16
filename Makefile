# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors

PKG := github.com/ianglennon/bayplan/internal/buildinfo

# The three Go modules: the server (the root), the agent and the shared wire
# protocol they both import. Every Go gate (test, lint, tidy-check) loops over
# them, so a new module is covered the day it is added to this list.
GO_MODULES := . agent proto

# Set VERSION (and optionally COMMIT) for a release build, for example
#   make build VERSION=v0.1.0
# Without VERSION the binary reports "dev (<commit>)" from Go's VCS data.
LDFLAGS :=
ifneq ($(VERSION),)
LDFLAGS += -X $(PKG).Version=$(VERSION)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS += -X $(PKG).Commit=$(COMMIT)
endif
ifneq ($(SOURCE_URL),)
LDFLAGS += -X $(PKG).SourceURL=$(SOURCE_URL)
endif

.PHONY: web build gen gen-check tidy-check test test-int test-swarm smoke dev dev-go dev-web dev-swarm dev-swarm-down spdx licences dco hooks test-scripts lint-config lint actionlint release-check image image-smoke agent-image agent-image-smoke edge-context e2e e2e-image e2e-copy proxy-smoke upgrade-smoke restore-smoke

web:
	pnpm -C web install --frozen-lockfile
	pnpm -C web build
	touch web/dist/.gitkeep  # Vite empties outDir

build: web
	CGO_ENABLED=0 go build -trimpath $(if $(LDFLAGS),-ldflags "$(LDFLAGS)") -o bin/bayplan ./cmd/bayplan

# sqlc runs through `go run` at a pinned version, so nothing is added to go.mod.
SQLC_VERSION := v1.31.1

gen:
	go run ./cmd/bayplan openapi > openapi.json
	go run ./cmd/bayplan permissions --format markdown > docs/permissions.md
	go run ./cmd/bayplan permissions --format typescript > web/src/api/permissions.gen.ts
	pnpm -C web exec openapi-typescript ../openapi.json -o src/api/schema.d.ts
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate
	node scripts/gen-notices.mjs

gen-check: gen tidy-check
	git diff --exit-code -- openapi.json web/src/api/schema.d.ts internal/store/sqlc web/public/third-party-notices.json docs/permissions.md web/src/api/permissions.gen.ts

# go.mod and go.sum of every module must already be tidy: `go mod tidy -diff`
# prints what tidy would change and exits non-zero without touching the files.
tidy-check:
	@set -e; for m in $(GO_MODULES); do \
	  echo "tidy-check: $$m"; \
	  go -C $$m mod tidy -diff; \
	done

test:
	@set -e; for m in $(GO_MODULES); do \
	  echo "test: $$m"; \
	  go -C $$m test ./... -race -count=1 -shuffle=on -timeout 20m; \
	done
	pnpm -C web exec vitest run

# Real-engine tests (D-28), build tag `integration`. With BAYPLAN_TEST_DOCKER_HOST
# set (CI points it at the dind service) they use that engine; without it each
# run starts docker:<N>-dind (N is BAYPLAN_TEST_ENGINE, default 29) with testcontainers, so the
# machine needs a Docker it can reach. The agent tests always start their own
# dind (they need a network the in-process Bayplan is reachable on) and load the
# agent image into it, so the image is built first.
test-int: agent-image
	go test -tags=integration -race -count=1 -timeout 30m ./internal/...

# The multi-node suite (D-57, D-58), build tag `swarm`: real agents in a dind swarm
# of 3 managers and 1 worker, for the engine BAYPLAN_TEST_ENGINE names (default
# 29). Needs Docker it can reach, with buildx for the agent image. One engine takes
# about 17 minutes on a developer machine, so the 40 minute timeout (CI allows 45
# for the job) is a margin, not a target.
test-swarm: agent-image
	go test -tags=swarm -race -count=1 -timeout 40m ./internal/...

smoke:
	scripts/smoke.sh

# Dev loop (D-77): Go with live reload plus Vite with HMR. Open
# http://localhost:5173/; Vite proxies /api to the Go server on 127.0.0.1:7480.
#
# From another machine, make Vite listen beyond loopback and tell both servers the
# address the browser will use:
#   make dev DEV_WEB_HOST=0.0.0.0 DEV_PUBLIC_URL=http://<this machine's name>:5173
# and open that address. The public URL must be the address in the browser's bar,
# because the Origin check (D-36) compares the two and refuses every change from
# any other origin. DEV_PUBLIC_URL= (empty) leaves the public URL unset, so the
# setup wizard asks for it. Do not do this on a network you do not trust.
# DEV_LISTEN=0.0.0.0:7480 is a different thing: it lets the dev swarm's nodes reach
# the Go server (make dev-swarm), and a remote browser does not need it.
# The Go server on :7480 serves web/dist as the last `make web` or `pnpm -C web
# build` left it; the Vite dev server never writes web/dist.
# There is deliberately no dev flag or env var that skips the setup-token gate (D-79).
dev:
	$(MAKE) -j2 dev-go dev-web

# air is run via `go run`, never added to go.mod. All settings are command-line
# overrides, so there is no .air.toml. The devspa tag serves the SPA from
# web/dist on disk as it is at the moment (it is not rebuilt by Vite's dev server).
#
# The agent settings are for the local swarm (make dev-swarm) and for development
# only. BAYPLAN_AGENT_URL is where agents dial: the dev swarm's network sees this
# machine as DEV_SWARM_GATEWAY, and it is plain ws:// because there is no
# certificate for an address like that. BAYPLAN_AGENT_ALLOW_INSECURE lets those
# plain connections in, which sends join tokens and Docker answers unencrypted; the
# server logs a warning at start while it is on. While it is on, the command Bayplan
# shows also sets BAYPLAN_AGENT_ALLOW_INSECURE=true on the agents, which otherwise
# refuse plain ws:// to another host. Neither belongs anywhere but this
# target and the proxy smoke test (scripts/proxy-smoke.sh). To let the dev
# swarm's nodes reach the server, listen on every interface:
# make dev DEV_LISTEN=0.0.0.0:7480 (the default stays loopback).
DEV_LISTEN ?= 127.0.0.1:7480
DEV_PUBLIC_URL ?= http://localhost:5173
DEV_WEB_HOST ?=
DEV_SWARM_GATEWAY ?= 172.31.250.1
dev-go:
	BAYPLAN_LISTEN=$(DEV_LISTEN) BAYPLAN_DATA_DIR=./.devdata BAYPLAN_PUBLIC_URL='$(DEV_PUBLIC_URL)' \
	BAYPLAN_AGENT_URL=http://$(DEV_SWARM_GATEWAY):7480 BAYPLAN_AGENT_ALLOW_INSECURE=true \
	go run github.com/air-verse/air@v1.67.4 \
		-tmp_dir .devdata/air \
		-build.cmd "go build -tags devspa -o .devdata/air/bayplan ./cmd/bayplan" \
		-build.bin .devdata/air/bayplan \
		-build.args_bin serve \
		-build.include_ext go \
		-build.exclude_dir web,bin,.devdata,.git,.planning,.claude,scripts

# BAYPLAN_DEV_PUBLIC_URL lets the public URL's host name past Vite's host check
# (web/vite.config.ts); without it Vite answers only localhost and IP addresses.
dev-web:
	BAYPLAN_DEV_PUBLIC_URL='$(DEV_PUBLIC_URL)' pnpm -C web dev $(if $(DEV_WEB_HOST),--host $(DEV_WEB_HOST))

# A local Docker Swarm of three managers and a worker (docker:29-dind, privileged),
# with the agent built from this tree loaded on every node (D-60). The command it
# prints next uses make dev. dev-swarm-down removes exactly what dev-swarm created,
# by name, and never prunes. Needs Docker with buildx; DEV_SWARM_ENGINE picks the
# dind major version.
dev-swarm:
	scripts/dev-swarm.sh up

dev-swarm-down:
	scripts/dev-swarm.sh down

# --- Policy gates (D-68 to D-70, D-74) ---

# D-68: every .go/.ts/.tsx/.sql file carries the two SPDX lines.
spdx:
	scripts/spdx-check.sh

# D-70: Go (linux/amd64 and linux/arm64) and production pnpm dependency licences.
licences:
	scripts/licences-check.sh

# D-69: every non-merge commit in the range is signed off. CI overrides the range.
DCO_BASE ?= origin/main
DCO_HEAD ?= HEAD
dco:
	scripts/dco-check.sh $(DCO_BASE) $(DCO_HEAD)

# Installs the prepare-commit-msg hook that adds Signed-off-by automatically.
hooks:
	git config core.hooksPath scripts/git-hooks

# Fail-first proof: each gate fails on a seeded violation and passes on a clean fixture.
test-scripts:
	scripts/test-scripts.sh

# --- Lint (D-74) ---
# Pinned tool versions. All run through `go run`, so nothing is added to go.mod
# and the linter is built with the local Go toolchain (Pitfall 24).
GOLANGCI_LINT_VERSION := v2.14.0
ACTIONLINT_VERSION := v1.7.12
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint-config:
	$(GOLANGCI_LINT) config verify

# golangci-lint has no -C flag, so each module is linted from inside a subshell.
# The root module's ./... stops at the nested go.mod files, so no package is
# linted twice. The TLS gate keeps the switch that turns off certificate
# verification in the agent's one reviewed file (D-06).
lint:
	@set -e; for m in $(GO_MODULES); do \
	  echo "lint: $$m"; \
	  (cd $$m && $(GOLANGCI_LINT) run ./...); \
	done
	scripts/insecure-tls-check.sh
	pnpm -C web exec biome ci .
	pnpm -C web exec tsc --noEmit

actionlint:
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) .github/workflows/*.yml

# --- Release (D-62, D-75) ---
# goreleaser runs through `go run` at the pinned version, so nothing is added to
# go.mod. The first run compiles it, which takes a few minutes.
GORELEASER_VERSION := v2.18.2
GORELEASER := go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

# Validates .goreleaser.yaml, builds this machine's server and agent binaries the
# way a release does (SPA, ldflags, trimpath) and runs each one's version output.
# Needs no Docker.
release-check:
	$(GORELEASER) check
	@mkdir -p bin
	$(GORELEASER) build --snapshot --clean --single-target --id bayplan --output bin/bayplan-release
	@v="$$(bin/bayplan-release version)"; echo "$$v"; \
	case "$$v" in \
	  v[0-9]*) ;; \
	  *) echo "release-check: the release binary reports '$$v', expected a version that starts with v and a number" >&2; exit 1 ;; \
	esac
	$(GORELEASER) build --snapshot --clean --single-target --id bayplan-agent --output bin/bayplan-agent-release
	@v="$$(bin/bayplan-agent-release --version)"; echo "$$v"; \
	case "$$v" in \
	  v[0-9]*) ;; \
	  *) echo "release-check: the release agent reports '$$v', expected a version that starts with v and a number" >&2; exit 1 ;; \
	esac

# The release image for this machine's engine, built exactly as a release builds
# it but not pushed, signed or SBOM'd. Needs Docker with buildx.
image:
	$(GORELEASER) release --snapshot --clean --skip=publish,sign,sbom

# Runs the image `make image` built: healthy, UID 1000, setup-token through
# docker exec, state kept across docker rm -f, and its own container ID. Needs Docker.
image-smoke:
	scripts/image-smoke.sh

# The agent image for this machine's architecture, tagged AGENT_TAG (default
# bayplan-agent:dev) and loaded into the local engine. The context is laid out as goreleaser lays it
# out: $TARGETPLATFORM/bayplan-agent plus agent/image/state/. The binary is a
# plain build (version "dev"); set AGENT_VERSION=v0.1.0 to stamp one. The
# self-update tests build two, make agent-image AGENT_VERSION=v0.2.0
# AGENT_TAG=bayplan-agent:v0.2.0 and the same for v0.2.1. Needs Docker with buildx.
AGENT_ARCH ?= $(shell go env GOARCH)
AGENT_CTX := .devdata/agent-image
AGENT_VERSION_PKG := github.com/ianglennon/bayplan/agent/internal/version
AGENT_TAG ?= bayplan-agent:dev
agent-image:
	rm -rf $(AGENT_CTX)
	mkdir -p $(AGENT_CTX)/linux/$(AGENT_ARCH) $(AGENT_CTX)/agent/image
	cp -R agent/image/state $(AGENT_CTX)/agent/image/state
	CGO_ENABLED=0 GOOS=linux GOARCH=$(AGENT_ARCH) go -C agent build -trimpath \
		-ldflags "-s -w $(if $(AGENT_VERSION),-X $(AGENT_VERSION_PKG).Version=$(AGENT_VERSION))" \
		-o $(CURDIR)/$(AGENT_CTX)/linux/$(AGENT_ARCH)/bayplan-agent ./cmd/bayplan-agent
	docker buildx build --platform linux/$(AGENT_ARCH) -f agent/Dockerfile -t $(AGENT_TAG) --load $(AGENT_CTX)

# Runs bayplan-agent:dev with the documented hardening flags: UID 1000, no
# root capability left, no-new-privileges (also without the security option, as
# a swarm service runs it), a writable state volume and the Docker socket. Needs
# Docker, python3 and tar.
agent-image-smoke:
	scripts/agent-image-smoke.sh

# The build contexts for the :edge images: the binaries laid out as
# $TARGETPLATFORM/bayplan (server) and $TARGETPLATFORM/bayplan-agent (agent)
# next to their Dockerfiles, as goreleaser does it. CI pushes them with buildx
# from the main branch.
edge-context:
	$(GORELEASER) build --snapshot --clean
	rm -rf dist/edge dist/edge-agent
	mkdir -p dist/edge/linux/amd64 dist/edge/linux/arm64
	cp "$$(find dist -path 'dist/edge*' -prune -o -type f -name bayplan -path '*linux_amd64*' -print | head -n 1)" dist/edge/linux/amd64/bayplan
	cp "$$(find dist -path 'dist/edge*' -prune -o -type f -name bayplan -path '*linux_arm64*' -print | head -n 1)" dist/edge/linux/arm64/bayplan
	cp Dockerfile dist/edge/Dockerfile
	mkdir -p dist/edge-agent/linux/amd64 dist/edge-agent/linux/arm64 dist/edge-agent/agent/image
	cp "$$(find dist -path 'dist/edge*' -prune -o -type f -name bayplan-agent -path '*linux_amd64*' -print | head -n 1)" dist/edge-agent/linux/amd64/bayplan-agent
	cp "$$(find dist -path 'dist/edge*' -prune -o -type f -name bayplan-agent -path '*linux_arm64*' -print | head -n 1)" dist/edge-agent/linux/arm64/bayplan-agent
	cp -R agent/image/state dist/edge-agent/agent/image/state
	cp agent/Dockerfile dist/edge-agent/Dockerfile

# --- End-to-end (D-80, D-59, RESEARCH Pattern 11) ---
# The built image behind Caddy at https://127.0.0.1:8443/bayplan with a generated
# certificate, under Docker Compose with a docker:29-dind sidecar it manages and a
# second dind daemon that plays the remote host, driven by Playwright through
# setup, sign-in, the dashboard, the audit log, settings and the agent golden
# path (add an environment, run the copied command, switch to it, revoke it).
# Needs Docker with buildx and compose. Build outputs live under .devdata/, which
# is git-ignored. BAYPLAN_E2E_HTTPS_PORT moves the published port off 8443.
E2E_DIST := .devdata/dist-e2e
E2E_TLS := .devdata/e2e-tls
E2E_COMPOSE := docker compose -f e2e/compose.yaml
# --with-deps installs the OS libraries Chromium needs and may use sudo; drop it
# (E2E_BROWSER_ARGS=chromium) on a machine that already has them.
E2E_BROWSER_ARGS ?= --with-deps chromium

# The image under test: a linux/amd64 binary with the SPA embedded, wrapped by
# the release Dockerfile the way goreleaser does it ($TARGETPLATFORM/bayplan in
# the build context), tagged bayplan:e2e and loaded into the local engine.
e2e-image: web
	mkdir -p $(E2E_DIST)/linux/amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath $(if $(LDFLAGS),-ldflags "$(LDFLAGS)") -o $(E2E_DIST)/linux/amd64/bayplan ./cmd/bayplan
	docker buildx build --platform linux/amd64 -f Dockerfile -t bayplan:e2e --load $(E2E_DIST)

# Bayplan behind Caddy, nginx and Traefik, each with the file in docs/proxy, at
# the root of a host and at /bayplan: the index and its base meta, /api/version, a
# WebSocket upgrade of the agent endpoint, an unbuffered event stream, and three
# deliberately broken nginx setups that Bayplan must notice. Needs Docker, curl
# and jq; the proxy images are pulled when they are not local. One PASS or FAIL
# line per check; any FAIL fails the target.
proxy-smoke: e2e-image
	scripts/proxy-smoke.sh

# The upgrade proof (D-34, PLAT-02): run the previous build, put state into it (setup, an
# agent environment with a real agent connected, and a user, a team and a grant when
# that build has them), swap in the current image on the same volume and network alias,
# and check sign-in, the first admin's instance-admin flag, the agent coming back
# online by itself, the grant and the audit chain. One PASS or FAIL line per check.
#
# The baseline is UPGRADE_BASE_REF, a commit (CI passes the pull request's base, or the
# previous commit on a push). It is BUILT here from a detached git worktree with the
# same recipe as e2e-image, not pulled: there is no released image before v0.1.0 and
# the repository is private, so a registry has nothing to offer and a commit is the
# only baseline that is both available and honest. The worktree and the build context
# are removed on exit, even when the smoke fails. Needs Docker with buildx, pnpm, curl
# and jq, and the ref reachable in this clone (CI checks out with fetch-depth 0).
UPGRADE_BASE_REF ?= origin/main
UPGRADE_BASE_DIR := .devdata/upgrade-base
UPGRADE_BASE_CTX := .devdata/upgrade-base-ctx
upgrade-smoke: e2e-image agent-image
	@set -e; \
	cleanup() { git worktree remove --force $(UPGRADE_BASE_DIR) >/dev/null 2>&1 || true; git worktree prune; rm -rf $(UPGRADE_BASE_DIR) $(UPGRADE_BASE_CTX); }; \
	trap cleanup EXIT; \
	cleanup; \
	mkdir -p .devdata; \
	git worktree add --detach $(UPGRADE_BASE_DIR) '$(UPGRADE_BASE_REF)'; \
	pnpm -C $(UPGRADE_BASE_DIR)/web install --frozen-lockfile; \
	pnpm -C $(UPGRADE_BASE_DIR)/web build; \
	touch $(UPGRADE_BASE_DIR)/web/dist/.gitkeep; \
	mkdir -p $(UPGRADE_BASE_CTX)/linux/amd64; \
	(cd $(UPGRADE_BASE_DIR) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o $(CURDIR)/$(UPGRADE_BASE_CTX)/linux/amd64/bayplan ./cmd/bayplan); \
	docker buildx build --platform linux/amd64 -f $(UPGRADE_BASE_DIR)/Dockerfile -t bayplan:base --load $(UPGRADE_BASE_CTX); \
	BASE_IMAGE=bayplan:base HEAD_IMAGE=bayplan:e2e AGENT_IMAGE=bayplan-agent:dev scripts/upgrade-smoke.sh

# The restore-over-a-running-container proof (D-30, PLAT-03, RESEARCH A3/A5): in the
# shipped image with no restart policy, restore a backup through the web UI's API and
# check that Bayplan re-executed itself with PID 1 intact (same State.Pid, StartedAt and
# RestartCount), the restored accounts sign in, an account made after the backup does not,
# and the audit chain verifies. RESTART_MODE=exit proves the fallback (the process
# exits and a restart policy starts it) with the same checks. One PASS or FAIL line per
# check; the last line is RESTORE SMOKE PASS. Needs Docker, curl, jq and tar.
restore-smoke: e2e-image
	scripts/restore-smoke.sh

# The clipboard fallback in a real Chromium on an insecure origin (http://bayplan.test
# mapped to 127.0.0.1, so there is no navigator.clipboard), against the real
# CopyableCode plain, inside a Dialog and inside an AlertDialog (G-02-1a). jsdom
# cannot show that bug. Needs no Docker.
e2e-copy:
	pnpm -C web install --frozen-lockfile
	pnpm -C e2e install --frozen-lockfile
	pnpm -C e2e exec playwright install $(E2E_BROWSER_ARGS)
	pnpm -C e2e exec playwright test -c playwright.copy.config.ts

# The certificate the proxy serves is generated here (7 days, SANs 127.0.0.1,
# localhost and the proxy's address on the compose network) with its SPKI pin,
# which the agent spec gives the agent as BAYPLAN_AGENT_PIN. The agent image is
# built and loaded into the remote host's Docker under the name the install
# command uses for an edge server, so the run needs no registry.
# The browser runs in the stack (the browser service, out of reach of the host's
# network changes), from the e2e packages installed first; the local install is
# for the copy suite and for running a spec by hand without the stack's browser.
# The stack is always torn down, with its volumes, whatever the tests do, and
# the exit status is the tests' own. A failed run prints the container logs.
E2E_BROWSER_WS = ws://127.0.0.1:$${BAYPLAN_E2E_BROWSER_PORT:-8444}/
e2e: e2e-image agent-image
	@set -e; \
	go run scripts/e2e-cert.go -out $(E2E_TLS); \
	pnpm -C e2e install --frozen-lockfile; \
	trap '$(E2E_COMPOSE) down -v' EXIT; \
	$(E2E_COMPOSE) up -d --wait || { $(E2E_COMPOSE) logs --no-color --tail 100; exit 1; }; \
	docker save bayplan-agent:dev | $(E2E_COMPOSE) exec -T agenthost docker load; \
	$(E2E_COMPOSE) exec -T agenthost docker tag bayplan-agent:dev ghcr.io/ianglennon/bayplan-agent:edge; \
	pnpm -C e2e exec playwright install $(E2E_BROWSER_ARGS); \
	status=0; \
	BAYPLAN_E2E_BROWSER_WS=$(E2E_BROWSER_WS) pnpm -C e2e exec playwright test || status=$$?; \
	if [ "$$status" -ne 0 ]; then \
		$(E2E_COMPOSE) logs --no-color --tail 100 bayplan || true; \
		$(E2E_COMPOSE) logs --no-color --tail 50 caddy || true; \
		$(E2E_COMPOSE) exec -T agenthost docker logs --tail 50 bayplan-agent || true; \
	fi; \
	exit $$status
