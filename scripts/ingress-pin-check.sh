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
#    The licence evidence ships with the image and names that same tag: the notice
#    (ingress/image/doc/NOTICE.md) must name only the pinned tag and record a full
#    commit, and ingress/image/doc/LICENSE.md must exist. A bump that leaves them
#    behind fails. With --check-clone the checked-out tag is compared too: its HEAD
#    must be the commit the notice records (a force-moved upstream tag changes it)
#    and its LICENSE.md must equal the shipped one.
# 2. The distroless "static" digest. Dockerfile, agent/Dockerfile and
#    ingress/Dockerfile share one base and are bumped together; a bump that
#    reaches only one or two of them fails.
#
# Usage: scripts/ingress-pin-check.sh [--print-tag] [--check-clone <traefik-clone>] [directory]
# The directory defaults to the repository root. With --print-tag, and only after
# every check has passed, the pinned Traefik tag (vX.Y.Z) is printed on standard
# output. With --check-clone <dir>, <dir> is a checkout of that tag (the licence gate
# clones it) and is checked against the notice and licence file. Messages go to
# standard error.
set -euo pipefail

PRINT_TAG=0
CLONE=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-tag)
      PRINT_TAG=1
      shift
      ;;
    --check-clone)
      if [ "$#" -lt 2 ] || [ -z "$2" ]; then
        echo "usage: ingress-pin-check.sh [--print-tag] [--check-clone <traefik-clone>] [directory]" >&2
        exit 2
      fi
      CLONE="$2"
      shift 2
      ;;
    *) break ;;
  esac
done
ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
# A relative clone path is read from where the script was started, not from ROOT.
if [ -n "${CLONE}" ]; then
  CLONE="$(cd "${CLONE}" && pwd)"
fi
cd "${ROOT}"

DOCKERFILE_TRAEFIK=ingress/Dockerfile
HARNESS=internal/ingressrender/traefiktest/traefiktest.go
NOTICE=ingress/image/doc/NOTICE.md
LICENCE_FILE=ingress/image/doc/LICENSE.md
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

# The notice and licence evidence belong to the pinned tag (WR-B-02). Every vX.Y.Z in
# the notice must be the pinned tag, and its Commit row must hold a full commit.
notice_commit=""
if [ -n "${from_pin}" ]; then
  tag="${from_pin#traefik:}"
  tag="${tag%%@*}"
  if [ ! -f "${NOTICE}" ]; then
    echo "PIN CHECK: ${NOTICE} is missing" >&2
    bad=1
  else
    versions="$(grep -Eo 'v[0-9]+\.[0-9]+\.[0-9]+' "${NOTICE}" | sort -u || true)"
    if [ -z "${versions}" ]; then
      echo "PIN CHECK: ${NOTICE} names no Traefik version; it must name ${tag}" >&2
      bad=1
    else
      for v in ${versions}; do
        if [ "${v}" != "${tag}" ]; then
          echo "PIN CHECK: ${NOTICE} names Traefik ${v} but ${DOCKERFILE_TRAEFIK} pins ${tag}; update the notice with the pin" >&2
          bad=1
        fi
      done
    fi
    notice_commit="$(sed -nE 's#^\|[[:space:]]*Commit[[:space:]]*\|[[:space:]]*([0-9a-f]{40})[[:space:]]*\|[[:space:]]*$#\1#p' "${NOTICE}" | head -n 1 || true)"
    if [ -z "${notice_commit}" ]; then
      echo "PIN CHECK: ${NOTICE} records no 40-character Commit row" >&2
      bad=1
    fi
  fi
  if [ ! -s "${LICENCE_FILE}" ]; then
    echo "PIN CHECK: ${LICENCE_FILE} is missing or empty" >&2
    bad=1
  fi
fi

# The checked-out tag against what the notice records and what ships.
if [ -n "${CLONE}" ]; then
  clone_commit="$(git -C "${CLONE}" rev-parse HEAD 2>/dev/null || true)"
  if [ -z "${clone_commit}" ]; then
    echo "PIN CHECK: ${CLONE} is not a git checkout" >&2
    bad=1
  elif [ -n "${notice_commit}" ] && [ "${clone_commit}" != "${notice_commit}" ]; then
    echo "PIN CHECK: the checked-out Traefik tag is commit ${clone_commit} but ${NOTICE} records ${notice_commit}; the tag moved or the notice is stale" >&2
    bad=1
  fi
  if [ ! -f "${CLONE}/LICENSE.md" ]; then
    echo "PIN CHECK: ${CLONE} has no LICENSE.md to compare with ${LICENCE_FILE}" >&2
    bad=1
  elif [ -s "${LICENCE_FILE}" ] && ! cmp -s "${CLONE}/LICENSE.md" "${LICENCE_FILE}"; then
    echo "PIN CHECK: ${LICENCE_FILE} differs from LICENSE.md of the pinned Traefik tag; copy it again" >&2
    bad=1
  fi
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
  echo "${tag}"
fi
