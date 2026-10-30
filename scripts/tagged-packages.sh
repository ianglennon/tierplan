#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# Prints, one import path per line, the packages under ./internal/... whose test
# files differ when the build tag TAG is on. `make test-int` and `make test-swarm`
# run only these, because every other package's tests are the unit suite that
# `make test` already ran; running them again under a tag cost about 200
# job-minutes per CI run (06.1 RESEARCH, CI audit finding 1). The list is derived,
# never written down, so a newly tagged package cannot be missed, and an empty
# list is an error so a typo in a tag cannot turn the target into a no-op.
#
# Usage: scripts/tagged-packages.sh TAG
# Exit codes: 0 list printed, 1 no package has tests under the tag, 2 usage.
set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: scripts/tagged-packages.sh TAG" >&2
  exit 2
fi
TAG="$1"

cd "$(cd "$(dirname "$0")/.." && pwd)"

list() {
  go list "$@" -f '{{.ImportPath}} {{.TestGoFiles}} {{.XTestGoFiles}}' ./internal/... | sort
}

plain="$(list)"
tagged="$(list -tags="${TAG}")"

# A line only in the tagged listing is a package whose test file list changed.
changed="$(comm -13 <(printf '%s\n' "${plain}") <(printf '%s\n' "${tagged}") | awk '{print $1}')"

if [ -z "${changed}" ]; then
  echo "tagged-packages: no package has tests under the build tag ${TAG}" >&2
  exit 1
fi
printf '%s\n' "${changed}"
