#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# The first step of every Docker-heavy CI job (D-23): prints the free space of the
# workspace and of Docker's root directory, and `docker system df`, then fails with
# the path and the free space named when either has less than TIERPLAN_CI_MIN_FREE_GB
# (default 20) free. Two runners share each VM's disk and Docker daemon, so a full
# disk is reported here as a full disk and not as a confusing test failure later.
#
# It also fails when git or git-http-backend is missing, because the git fixture tests
# need them and a skipped test would pass the git code unchecked (Phase 9).
#
# Docker's root directory is usually readable only by root, so the check measures
# the nearest parent the runner user can stat; it is on the same filesystem.
set -euo pipefail

MIN="${TIERPLAN_CI_MIN_FREE_GB:-20}"
case "${MIN}" in
  '' | *[!0-9]*)
    echo "ci-preflight: TIERPLAN_CI_MIN_FREE_GB must be a whole number of GB, not '${MIN}'" >&2
    exit 2 ;;
esac
command -v docker >/dev/null || { echo "ci-preflight: docker is required" >&2; exit 1; }

ROOT_DIR="$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || true)"
if [ -z "${ROOT_DIR}" ]; then
  echo "ci-preflight: the Docker daemon does not answer; is the runner user in the docker group?" >&2
  exit 1
fi

failed=0
# check_free LABEL PATH prints the filesystem line and compares its free space.
check_free() {
  local label="$1" path="$2" probe="$2" avail_kb free_gb
  # The nearest ancestor that df can read.
  while ! df -Pk "${probe}" >/dev/null 2>&1; do
    [ "${probe}" != "/" ] || break
    probe="$(dirname "${probe}")"
  done
  echo "ci-preflight: ${label} (${path}):"
  df -h "${probe}" | sed 's/^/  /'
  avail_kb="$(df -Pk "${probe}" | awk 'NR == 2 { print $4 }')"
  case "${avail_kb}" in
    '' | *[!0-9]*) echo "ci-preflight: cannot read the free space of ${probe}" >&2; failed=1; return ;;
  esac
  free_gb=$((avail_kb / 1048576))
  if [ "${free_gb}" -lt "${MIN}" ]; then
    echo "ci-preflight: ${path} has ${free_gb} GB free on $(df -Pk "${probe}" | awk 'NR == 2 { print $1 " (" $6 ")" }'), below TIERPLAN_CI_MIN_FREE_GB=${MIN}" >&2
    failed=1
  fi
}

check_free "workspace" "$(pwd)"
check_free "Docker root" "${ROOT_DIR}"
echo "ci-preflight: docker system df:"
docker system df | sed 's/^/  /'

if [ "${failed}" -ne 0 ]; then
  echo "ci-preflight: not enough free disk; see the lines above (a weekly prune runs on the runner VMs; free space by hand on this one)" >&2
  exit 1
fi
# The git fixture tests (internal/gitx/gittest) serve real repositories with git-http-backend.
# A runner without git must fail them loudly: a skipped test would pass the git code unchecked.
if ! git --version >/dev/null 2>&1 \
  || [ ! -x "$(git --exec-path 2>/dev/null)/git-http-backend" ]; then
  echo "ci-preflight: git and git-http-backend are required for the git fixture tests (internal/gitx/gittest)" >&2
  exit 1
fi
echo "ci-preflight: $(git --version), git-http-backend present"

echo "ci-preflight: OK (at least ${MIN} GB free)"
