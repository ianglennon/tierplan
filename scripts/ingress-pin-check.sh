#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Keeps the two pins of the ingress image single (D-08, Pitfall 9, T-23-39, T-23-41).
#
# 1. The Traefik reference (tag and index digest). It is written in two places
#    that must agree: the `FROM traefik:... AS traefik` line of ingress/Dockerfile
#    (what ships) and the TraefikImage constant of
#    internal/ingressrender/traefiktest/traefiktest.go (what the tests run). The
#    licence gate reads the tag from here, so it scans what ships.
# 2. The distroless "static" digest. Dockerfile, agent/Dockerfile and
#    ingress/Dockerfile share one base and are bumped together; a bump that
#    reaches only one or two of them fails.
#
# Usage: scripts/ingress-pin-check.sh [--print-tag] [directory]
# The directory defaults to the repository root. With --print-tag, and only after
# every check has passed, the pinned Traefik tag (vX.Y.Z) is printed on standard
# output. Messages go to standard error.
set -euo pipefail

PRINT_TAG=0
if [ "${1:-}" = "--print-tag" ]; then
  PRINT_TAG=1
  shift
fi
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "${ROOT}"

DOCKERFILE_TRAEFIK=ingress/Dockerfile
HARNESS=internal/ingressrender/traefiktest/traefiktest.go
BASES="Dockerfile agent/Dockerfile ingress/Dockerfile"

REF='traefik:v[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}'
BASE_REF='gcr\.io/distroless/static-debian13@sha256:[0-9a-f]{64}'

bad=0

from_pin="$(sed -nE "s#^FROM (${REF}) AS traefik[[:space:]]*\$#\\1#p" "${DOCKERFILE_TRAEFIK}" 2>/dev/null | head -n 1 || true)"
harness_pin="$(sed -nE "s#^[[:space:]]*TraefikImage[[:space:]]*=[[:space:]]*\"(${REF})\".*\$#\\1#p" "${HARNESS}" 2>/dev/null | head -n 1 || true)"

if [ -z "${from_pin}" ]; then
  echo "PIN CHECK: ${DOCKERFILE_TRAEFIK} holds no Traefik reference" >&2
  bad=1
fi
if [ -z "${harness_pin}" ]; then
  echo "PIN CHECK: ${HARNESS} holds no Traefik reference" >&2
  bad=1
fi
if [ -n "${from_pin}" ] && [ -n "${harness_pin}" ] && [ "${from_pin}" != "${harness_pin}" ]; then
  echo "PIN CHECK: ${DOCKERFILE_TRAEFIK} pins ${from_pin} but ${HARNESS} pins ${harness_pin}" >&2
  bad=1
fi

first_file=""
first_digest=""
for f in ${BASES}; do
  d="$(sed -nE "s#^FROM (${BASE_REF})([[:space:]].*)?\$#\\1#p" "${f}" 2>/dev/null | head -n 1 || true)"
  if [ -z "${d}" ]; then
    echo "PIN CHECK: ${f} holds no distroless static FROM line with a digest" >&2
    bad=1
    continue
  fi
  if [ -z "${first_file}" ]; then
    first_file="${f}"
    first_digest="${d}"
  elif [ "${d}" != "${first_digest}" ]; then
    echo "PIN CHECK: ${f} uses ${d} but ${first_file} uses ${first_digest}; bump the distroless digest in all three Dockerfiles together" >&2
    bad=1
  fi
done

[ "${bad}" -eq 0 ] || exit 1

if [ "${PRINT_TAG}" -eq 1 ]; then
  tag="${from_pin#traefik:}"
  echo "${tag%%@*}"
fi
