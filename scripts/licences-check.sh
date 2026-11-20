#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Licence gate (D-70). Checks the Go dependencies of the server binary on both
# release architectures, then the production pnpm dependencies of the SPA, against
# the allowlist in scripts/licence-check.mjs and the reviewed exceptions in
# licence-exceptions.txt.
#
# The web SBOM is generated and compared with pnpm's licence list as an exact
# name@version set (D-27, scripts/web-sbom.sh). The licences job runs on every pull
# request, so that set check runs whenever web/package.json or the lockfile changes,
# and on every other change too.
#
# go-licenses reports only what the target package imports, so test-only
# dependencies are excluded. The project's own module is ignored because
# AGPL-3.0 is not a dependency licence.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

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
    "${TOOLS}/go-licenses" report ./cmd/tierplan --ignore github.com/ianglennon/tierplan \
    | node scripts/licence-check.mjs --format go-csv
done

# The agent is a separate module with its own dependency graph (yamux,
# websocket), shipped as its own binary on both architectures. The shared proto
# module is standard library only and is covered through the agent.
for arch in amd64 arm64; do
  echo "licences: agent go linux/${arch}"
  (cd agent && CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
    "${TOOLS}/go-licenses" report ./cmd/tierplan-agent --ignore github.com/ianglennon/tierplan \
    | node ../scripts/licence-check.mjs --format go-csv)
done

# Traefik, from the tag held by the pinned image reference. The pin is read, never
# copied here, so the gate scans what the test harness and the image use.
tag="$(sed -nE 's/^[[:space:]]*TraefikImage[[:space:]]*=[[:space:]]*"traefik:(v[0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}".*$/\1/p' \
  internal/ingressrender/traefiktest/traefiktest.go | head -n 1)"
if [ -z "${tag}" ]; then
  echo "licences: cannot read the Traefik tag from internal/ingressrender/traefiktest/traefiktest.go" >&2
  exit 1
fi
git clone -q --depth 1 --branch "${tag}" https://github.com/traefik/traefik "${TOOLS}/traefik"
echo "licences: traefik ${tag} commit $(git -C "${TOOLS}/traefik" rev-parse HEAD)"
for arch in amd64 arm64; do
  echo "licences: traefik go linux/${arch}"
  (cd "${TOOLS}/traefik" && CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
    "${TOOLS}/go-licenses" report ./cmd/traefik \
    | node "${ROOT}/scripts/licence-check.mjs" --format go-csv)
done

echo "licences: pnpm (production)"
pnpm -C web licenses list --json --prod | node scripts/licence-check.mjs --format pnpm-json

echo "licences: web SBOM against pnpm licenses (D-27)"
scripts/web-sbom.sh "${TOOLS}/web.spdx.json"

echo "licences: OK"
