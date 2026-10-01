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

.PHONY: web build gen gen-check test smoke dev dev-go dev-web

web:
	pnpm -C web install --frozen-lockfile
	pnpm -C web build
	touch web/dist/.gitkeep  # Vite empties outDir

build: web
	CGO_ENABLED=0 go build -trimpath $(if $(LDFLAGS),-ldflags "$(LDFLAGS)") -o bin/bayplan ./cmd/bayplan

gen:
	go run ./cmd/bayplan openapi > openapi.json
	pnpm -C web exec openapi-typescript ../openapi.json -o src/api/schema.d.ts

gen-check: gen
	git diff --exit-code -- openapi.json web/src/api/schema.d.ts

test:
	go test ./... -race -count=1
	pnpm -C web exec vitest run

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
