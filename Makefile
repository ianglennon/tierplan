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

.PHONY: web build gen gen-check tidy-check test test-int test-swarm smoke dev dev-go dev-web spdx licences dco hooks test-scripts lint-config lint actionlint release-check image image-smoke agent-image agent-image-smoke edge-context e2e e2e-image

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
	pnpm -C web exec openapi-typescript ../openapi.json -o src/api/schema.d.ts
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate
	node scripts/gen-notices.mjs

gen-check: gen tidy-check
	git diff --exit-code -- openapi.json web/src/api/schema.d.ts internal/store/sqlc web/public/third-party-notices.json

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
	  go -C $$m test ./... -race -count=1; \
	done
	pnpm -C web exec vitest run

# Real-engine tests (D-28), build tag `integration`. With BAYPLAN_TEST_DOCKER_HOST
# set (CI points it at the dind service) they use that engine; without it each
# run starts docker:<N>-dind (N is BAYPLAN_TEST_ENGINE, default 29) with testcontainers, so the
# machine needs a Docker it can reach. The agent tests always start their own
# dind (they need a network the in-process Bayplan is reachable on) and load the
# agent image into it, so the image is built first.
test-int: agent-image
	go test -tags=integration -race -count=1 ./internal/...

# The multi-node suite (D-57, D-58), build tag `swarm`: real agents in a dind swarm
# of 3 managers and 1 worker, for the engine BAYPLAN_TEST_ENGINE names (default
# 29). Needs Docker it can reach, with buildx for the agent image.
test-swarm: agent-image
	go test -tags=swarm -race -count=1 -timeout 30m ./internal/...

smoke:
	scripts/smoke.sh

# Dev loop (D-77): Go with live reload plus Vite with HMR. Open
# http://localhost:5173/; Vite proxies /api to the Go server on 127.0.0.1:7480.
# There is deliberately no dev flag or env var that skips the setup-token gate (D-79).
dev:
	$(MAKE) -j2 dev-go dev-web

# air is run via `go run`, never added to go.mod. All settings are command-line
# overrides, so there is no .air.toml. The devspa tag serves the SPA from
# web/dist on disk, so Vite rebuilds are visible without recompiling Go.
dev-go:
	BAYPLAN_LISTEN=127.0.0.1:7480 BAYPLAN_DATA_DIR=./.devdata BAYPLAN_PUBLIC_URL=http://localhost:5173 \
	go run github.com/air-verse/air@v1.67.4 \
		-tmp_dir .devdata/air \
		-build.cmd "go build -tags devspa -o .devdata/air/bayplan ./cmd/bayplan" \
		-build.bin .devdata/air/bayplan \
		-build.args_bin serve \
		-build.include_ext go \
		-build.exclude_dir web,bin,.devdata,.git,.planning,.claude,scripts

dev-web:
	pnpm -C web dev

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

# The agent image for this machine's architecture, tagged bayplan-agent:dev and
# loaded into the local engine. The context is laid out as goreleaser lays it
# out: $TARGETPLATFORM/bayplan-agent plus agent/image/state/. The binary is a
# plain build (version "dev"); set AGENT_VERSION=v0.1.0 to stamp one. Needs
# Docker with buildx.
AGENT_ARCH ?= $(shell go env GOARCH)
AGENT_CTX := .devdata/agent-image
AGENT_VERSION_PKG := github.com/ianglennon/bayplan/agent/internal/version
agent-image:
	rm -rf $(AGENT_CTX)
	mkdir -p $(AGENT_CTX)/linux/$(AGENT_ARCH) $(AGENT_CTX)/agent/image
	cp -R agent/image/state $(AGENT_CTX)/agent/image/state
	CGO_ENABLED=0 GOOS=linux GOARCH=$(AGENT_ARCH) go -C agent build -trimpath \
		-ldflags "-s -w $(if $(AGENT_VERSION),-X $(AGENT_VERSION_PKG).Version=$(AGENT_VERSION))" \
		-o $(CURDIR)/$(AGENT_CTX)/linux/$(AGENT_ARCH)/bayplan-agent ./cmd/bayplan-agent
	docker buildx build --platform linux/$(AGENT_ARCH) -f agent/Dockerfile -t bayplan-agent:dev --load $(AGENT_CTX)

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

# --- End-to-end (D-80, RESEARCH Pattern 11) ---
# The built image under Docker Compose with a docker:29-dind sidecar, driven by
# Playwright through setup, sign-in, the dashboard and the audit log. Needs
# Docker with buildx and compose. Build outputs live under .devdata/, which is
# git-ignored. BAYPLAN_E2E_PORT moves the published port off 7480.
E2E_DIST := .devdata/dist-e2e
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

# The stack is always torn down, with its volumes, whatever the tests do, and
# the exit status is the tests' own. A failed run prints the container logs.
e2e: e2e-image
	@set -e; \
	trap '$(E2E_COMPOSE) down -v' EXIT; \
	$(E2E_COMPOSE) up -d --wait || { $(E2E_COMPOSE) logs --no-color --tail 100; exit 1; }; \
	pnpm -C e2e install --frozen-lockfile; \
	pnpm -C e2e exec playwright install $(E2E_BROWSER_ARGS); \
	status=0; \
	pnpm -C e2e exec playwright test || status=$$?; \
	if [ "$$status" -ne 0 ]; then $(E2E_COMPOSE) logs --no-color --tail 100 bayplan || true; fi; \
	exit $$status
