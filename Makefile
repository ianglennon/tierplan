# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors

PKG := github.com/ianglennon/bayplan/internal/buildinfo

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

.PHONY: web build gen gen-check test test-int smoke dev dev-go dev-web spdx licences dco hooks test-scripts lint-config lint actionlint

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

gen-check: gen
	git diff --exit-code -- openapi.json web/src/api/schema.d.ts internal/store/sqlc web/public/third-party-notices.json

test:
	go test ./... -race -count=1
	pnpm -C web exec vitest run

# Real-engine tests (D-28), build tag `integration`. With BAYPLAN_TEST_DOCKER_HOST
# set (CI points it at the dind service) they use that engine; without it each
# run starts docker:<N>-dind (N is BAYPLAN_TEST_ENGINE, default 29) with testcontainers, so the
# machine needs a Docker it can reach.
test-int:
	go test -tags=integration -race -count=1 ./internal/...

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

lint:
	$(GOLANGCI_LINT) run ./...
	pnpm -C web exec biome ci .
	pnpm -C web exec tsc --noEmit

actionlint:
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) .github/workflows/*.yml
