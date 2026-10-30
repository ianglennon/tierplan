#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# Licence gate (D-70). Checks the Go dependencies of the server binary on both
# release architectures, then the production pnpm dependencies of the SPA, against
# the allowlist in scripts/licence-check.mjs and the reviewed exceptions in
# licence-exceptions.txt.
#
# go-licenses reports only what the target package imports, so test-only
# dependencies are excluded. The project's own module is ignored because
# AGPL-3.0 is not a dependency licence.
set -euo pipefail

cd "$(dirname "$0")/.."

GO_LICENSES="github.com/google/go-licenses/v2@v2.0.1"

# `go run` with GOARCH set would compile go-licenses itself for the target
# architecture and then fail to execute it ("exec format error" for arm64 on an
# amd64 host). Build the tool once for the host, then point it at each target
# through the environment. The tool is never added to go.mod.
TOOLS="$(mktemp -d)"
trap 'rm -rf "${TOOLS}"' EXIT
GOBIN="${TOOLS}" go install "${GO_LICENSES}"

for arch in amd64 arm64; do
  echo "licences: go linux/${arch}"
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
    "${TOOLS}/go-licenses" report ./cmd/coaming --ignore github.com/ianglennon/coaming \
    | node scripts/licence-check.mjs --format go-csv
done

# The agent is a separate module with its own dependency graph (yamux,
# websocket), shipped as its own binary on both architectures. The shared proto
# module is standard library only and is covered through the agent.
for arch in amd64 arm64; do
  echo "licences: agent go linux/${arch}"
  (cd agent && CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
    "${TOOLS}/go-licenses" report ./cmd/coaming-agent --ignore github.com/ianglennon/coaming \
    | node ../scripts/licence-check.mjs --format go-csv)
done

echo "licences: pnpm (production)"
pnpm -C web licenses list --json --prod | node scripts/licence-check.mjs --format pnpm-json

echo "licences: OK"
