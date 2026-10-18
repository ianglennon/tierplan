#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Removes the Docker containers, networks and volumes a CI job (or a local smoke run)
# created, so a failed or cancelled job leaves nothing on the VM's shared daemon (D-23).
# Every smoke script labels what it creates with bayplan.ci.owner=$BAYPLAN_CI_OWNER; this
# removes exactly the resources that carry that label value, in the order containers,
# networks, volumes, and prints each one it removed.
#
# BAYPLAN_CI_OWNER is required and the script exits 2 without it, so it can never be
# run in a way that removes another job's resources or a person's own. In ci.yml it is
# <run id>-<attempt>-<job>, built only from GitHub-generated values.
set -euo pipefail

OWNER="${BAYPLAN_CI_OWNER:-}"
if [ -z "${OWNER}" ]; then
  echo "ci-clean: BAYPLAN_CI_OWNER is not set; refusing to remove anything" >&2
  exit 2
fi
command -v docker >/dev/null || { echo "ci-clean: docker is required" >&2; exit 1; }

FILTER="label=bayplan.ci.owner=${OWNER}"
removed=0

# remove KIND NAME-FIELD [RM-FLAG...]: lists the resources of KIND with the owner label
# and removes each; one that is already gone is not an error. Containers are listed
# with -a (a stopped one still holds its name and its network).
remove() {
  local kind="$1" field="$2" id
  shift 2
  local -a ls_args=()
  [ "${kind}" = container ] && ls_args=(-a)
  while IFS= read -r id; do
    [ -n "${id}" ] || continue
    if docker "${kind}" rm "$@" "${id}" >/dev/null 2>&1; then
      echo "ci-clean: removed ${kind} ${id}"
      removed=$((removed + 1))
    else
      echo "ci-clean: could not remove ${kind} ${id}" >&2
    fi
  done < <(docker "${kind}" ls ${ls_args[@]+"${ls_args[@]}"} --filter "${FILTER}" --format "{{.${field}}}" 2>/dev/null || true)
}

remove container Names -f -v
remove network Name
remove volume Name -f

echo "ci-clean: ${removed} resource(s) removed for owner ${OWNER}"
