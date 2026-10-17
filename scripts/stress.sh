#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# The stability bars of phase 6.1, applied to one share of the suite (D-01, D-02,
# D-03, D-12, D-16). A shard is a set of Go packages, and the agent-web shard also
# carries the web bar. Every Go package of the three modules is in exactly one shard:
#
#   api        ./internal/api
#   core       ./internal/server/... ./internal/tunnel/... ./internal/envs/... ./internal/agents
#   rest       every other package of `go list ./...` in the root module (computed,
#              so a new package is never missed)
#   agent-web  every package of the agent and proto modules, plus WEB_RUNS shuffled
#              Vitest runs with the seeds 1 to WEB_RUNS
#
# Each Go package set runs three passes, each through gotestsum with its own JSON file
# in $TEST_RESULTS (stress-SHARD-PASS.json; the agent-web shard adds -MODULE):
#   count  -race -count=$STRESS_COUNT (20) -shuffle=on, plus the flags given (they
#          carry -timeout), the D-01 bar
#   cpu1   -race -count=1 -shuffle=on -cpu=1 -timeout 60m, the D-02 bar
#   cpu4   the same with -cpu=4
# Tests quarantined under D-12 are included (BAYPLAN_RUN_QUARANTINED=1): the point
# of the stress run is to find out whether they are stable yet. Every pass runs even
# when an earlier one failed, so one red run reports everything; the exit status is
# non-zero at the end if any pass failed. scripts/test-summary.sh turns the JSON into
# "failed k/N" lines.
#
# Usage: scripts/stress.sh SHARD [go test flags...]
# Environment: STRESS_COUNT (20), WEB_RUNS (10), GOTESTSUM (the gotestsum command,
# default `gotestsum`), TEST_RESULTS (the results directory, default .devdata/test).
# Exit: 0 all passes green, 1 a pass failed, 2 usage error.
set -uo pipefail

SHARDS="api core rest agent-web"
usage() {
  echo "usage: scripts/stress.sh SHARD [go test flags...]   (SHARD is one of: ${SHARDS// /, })" >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
shard="$1"
shift
case " ${SHARDS} " in
  *" ${shard} "*) ;;
  *) echo "stress: unknown shard '${shard}'" >&2; usage ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

count="${STRESS_COUNT:-20}"
web_runs="${WEB_RUNS:-10}"
for v in "${count}" "${web_runs}"; do
  case "${v}" in '' | *[!0-9]*) echo "stress: STRESS_COUNT and WEB_RUNS must be numbers, got '${v}'" >&2; exit 2 ;; esac
done
results="${TEST_RESULTS:-.devdata/test}"
case "${results}" in /*) ;; *) results="${ROOT}/${results}" ;; esac
mkdir -p "${results}"
rm -f "${results}/stress-${shard}-"*

# shellcheck disable=SC2206 # the command is a word list on purpose
gotestsum_cmd=(${GOTESTSUM:-gotestsum})

# D-12: run the quarantined tests too.
export BAYPLAN_RUN_QUARANTINED=1

failed=0

# run_pass NAME MODULE_DIR PKG...: one gotestsum pass over a package set.
# The pass flags come from the PASS_FLAGS array.
run_pass() {
  local name="$1" dir="$2"
  shift 2
  local file="${results}/stress-${shard}-${name}"
  [ "${dir}" = "." ] || file="${file}-$(basename "${dir}")"
  echo "stress: ${shard} pass ${name} in ${dir} (${PASS_FLAGS[*]})"
  if ! (cd "${dir}" && "${gotestsum_cmd[@]}" --format pkgname --jsonfile "${file}.json" -- "$@" "${PASS_FLAGS[@]}"); then
    echo "stress: ${shard} pass ${name} in ${dir} FAILED" >&2
    failed=1
  fi
}

# go_passes MODULE_DIR PKG...: the three passes over one module's packages.
go_passes() {
  local dir="$1"
  shift
  echo "stress: ${shard} packages in ${dir}:"
  printf '  %s\n' "$@"
  PASS_FLAGS=(-race "-count=${count}" -shuffle=on "${EXTRA_FLAGS[@]}")
  run_pass count "${dir}" "$@"
  PASS_FLAGS=(-race -count=1 -shuffle=on -cpu=1 -timeout 60m)
  run_pass cpu1 "${dir}" "$@"
  PASS_FLAGS=(-race -count=1 -shuffle=on -cpu=4 -timeout 60m)
  run_pass cpu4 "${dir}" "$@"
}

EXTRA_FLAGS=("$@")

root_pkgs() { go list "$@"; }

case "${shard}" in
  api)
    mapfile -t pkgs < <(root_pkgs ./internal/api)
    go_passes . "${pkgs[@]}"
    ;;
  core)
    mapfile -t pkgs < <(root_pkgs ./internal/server/... ./internal/tunnel/... ./internal/envs/... ./internal/agents)
    go_passes . "${pkgs[@]}"
    ;;
  rest)
    mapfile -t taken < <(root_pkgs ./internal/api ./internal/server/... ./internal/tunnel/... ./internal/envs/... ./internal/agents)
    mapfile -t all < <(root_pkgs ./...)
    pkgs=()
    for p in "${all[@]}"; do
      skip=0
      for t in "${taken[@]}"; do
        [ "${p}" = "${t}" ] && skip=1 && break
      done
      [ "${skip}" -eq 1 ] || pkgs+=("${p}")
    done
    go_passes . "${pkgs[@]}"
    ;;
  agent-web)
    for m in agent proto; do
      mapfile -t pkgs < <(cd "${m}" && go list ./...)
      go_passes "${m}" "${pkgs[@]}"
    done
    # D-03: the web bar, ten shuffled runs with seeds printed and kept beside the JSON.
    i=1
    while [ "${i}" -le "${web_runs}" ]; do
      echo "stress: web run ${i}/${web_runs}, seed ${i} (reproduce with make test-web WEB_SEED=${i})"
      echo "${i}" >"${results}/stress-web-${i}.seed"
      if ! pnpm -C web exec vitest run --sequence.shuffle "--sequence.seed=${i}" --reporter=default --reporter=json "--outputFile.json=${results}/stress-web-${i}.json"; then
        echo "stress: web run ${i} (seed ${i}) FAILED" >&2
        failed=1
      fi
      i=$((i + 1))
    done
    ;;
esac

if [ "${failed}" -ne 0 ]; then
  echo "stress: ${shard} FAILED (see the passes above and scripts/test-summary.sh)" >&2
  exit 1
fi
echo "stress: ${shard} OK"
