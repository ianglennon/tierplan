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
#   count  -race -count=$STRESS_COUNT (20) -shuffle=on, plus the flags given, the
#          D-01 bar
#   cpu1   -race -count=1 -shuffle=on -cpu=1, the D-02 bar
#   cpu4   the same with -cpu=4
#
# D-22: the whole shard runs inside ONE time budget, TEST_BUDGET=<N>m (make
# test-stress sets it from STRESS_BUDGET). The end is computed once, and every pass
# gets -timeout <the seconds left>s, so a hang ends in Go's goroutine dump and the
# passes cannot add up past the job's timeout-minutes (scripts/timeouts-check.sh
# keeps the job 10 minutes above the budget). A pass that finds no time left fails
# the shard; it is never skipped quietly. go test applies -timeout to each package's
# test binary, so a hang in a package that starts late can run past the budget by up
# to that package's own run; the job's margin covers it. A -timeout flag is refused,
# because the budget governs.
# Tests quarantined under D-12 are included (BAYPLAN_RUN_QUARANTINED=1): the point
# of the stress run is to find out whether they are stable yet. Every pass runs even
# when an earlier one failed, so one red run reports everything; the exit status is
# non-zero at the end if any pass failed. scripts/test-summary.sh turns the JSON into
# "failed k/N" lines.
#
# Usage: TEST_BUDGET=<N>m scripts/stress.sh SHARD [go test flags...]
# Environment: TEST_BUDGET (required, whole minutes as <N>m; 0m means no time left),
# STRESS_COUNT (20), WEB_RUNS (10), GOTESTSUM (the gotestsum command, default
# `gotestsum`), GO (the go command `go list` runs, default `go`), TEST_RESULTS (the
# results directory, default .devdata/test).
# Exit: 0 all passes green, 1 a pass failed or a package list could not be made,
# 2 usage error.
set -uo pipefail

SHARDS="api core rest agent-web"
usage() {
  echo "usage: TEST_BUDGET=<N>m scripts/stress.sh SHARD [go test flags...]   (SHARD is one of: ${SHARDS// /, })" >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
shard="$1"
shift
case " ${SHARDS} " in
  *" ${shard} "*) ;;
  *) echo "stress: unknown shard '${shard}'" >&2; usage ;;
esac

# The budget governs every pass's -timeout (D-22), so a caller's own is refused.
for a in "$@"; do
  case "${a}" in
    -timeout | -timeout=* | --timeout | --timeout=*)
      echo "stress: ${a} is not accepted: TEST_BUDGET governs the -timeout of every pass" >&2
      exit 2
      ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

count="${STRESS_COUNT:-20}"
web_runs="${WEB_RUNS:-10}"
for v in "${count}" "${web_runs}"; do
  case "${v}" in '' | *[!0-9]*) echo "stress: STRESS_COUNT and WEB_RUNS must be numbers, got '${v}'" >&2; exit 2 ;; esac
done
budget="${TEST_BUDGET:-}"
if [[ ! "${budget}" =~ ^[0-9]+m$ ]]; then
  echo "stress: TEST_BUDGET must be whole minutes as <N>m (make test-stress sets it from STRESS_BUDGET), got '${budget}'" >&2
  exit 2
fi
budget_minutes="${budget%m}"
# 10# so a leading zero is not read as octal.
end=$(($(date +%s) + 10#${budget_minutes} * 60))
seconds_left() { echo $((end - $(date +%s))); }
GO="${GO:-go}"
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
  local name="$1" dir="$2" left
  shift 2
  local file="${results}/stress-${shard}-${name}"
  [ "${dir}" = "." ] || file="${file}-$(basename "${dir}")"
  left="$(seconds_left)"
  if [ "${left}" -le 0 ]; then
    echo "stress: ${shard} budget of ${budget_minutes} minutes used up before pass ${name} in ${dir}" >&2
    failed=1
    return
  fi
  PASS_FLAGS+=("-timeout=${left}s")
  echo "stress: ${shard} pass ${name} in ${dir} (${PASS_FLAGS[*]}; ${left}s of the ${budget} budget left)"
  if ! (cd "${dir}" && "${gotestsum_cmd[@]}" --format pkgname --jsonfile "${file}.json" -- "$@" "${PASS_FLAGS[@]}"); then
    echo "stress: ${shard} pass ${name} in ${dir} FAILED" >&2
    failed=1
  fi
}

# go_passes MODULE_DIR PKG...: the three passes over one module's packages.
go_passes() {
  local dir="$1"
  shift
  if [ "$#" -eq 0 ]; then
    echo "stress: ${shard} has no package to test in ${dir}" >&2
    exit 1
  fi
  echo "stress: ${shard} packages in ${dir}:"
  printf '  %s\n' "$@"
  PASS_FLAGS=(-race "-count=${count}" -shuffle=on "${EXTRA_FLAGS[@]}")
  run_pass count "${dir}" "$@"
  PASS_FLAGS=(-race -count=1 -shuffle=on -cpu=1)
  run_pass cpu1 "${dir}" "$@"
  PASS_FLAGS=(-race -count=1 -shuffle=on -cpu=4)
  run_pass cpu4 "${dir}" "$@"
}

EXTRA_FLAGS=("$@")

# list_pkgs MODULE_DIR PATTERN...: go list into LISTED, one package per element. A
# failing go list or an empty list stops the shard (exit 1) naming the module: it
# is never passed on as a shorter list, because go test with no package argument
# runs only the current directory and the shard would report green over the rest.
list_pkgs() {
  local dir="$1" out
  shift
  if ! out="$(cd "${dir}" && "${GO}" list "$@")"; then
    echo "stress: go list failed for ${dir} $*" >&2
    exit 1
  fi
  LISTED=()
  if [ -n "${out}" ]; then mapfile -t LISTED <<<"${out}"; fi
  if [ "${#LISTED[@]}" -eq 0 ]; then
    echo "stress: go list found no package in ${dir} ($*)" >&2
    exit 1
  fi
}

case "${shard}" in
  api)
    list_pkgs . ./internal/api
    go_passes . "${LISTED[@]}"
    ;;
  core)
    list_pkgs . ./internal/server/... ./internal/tunnel/... ./internal/envs/... ./internal/agents
    go_passes . "${LISTED[@]}"
    ;;
  rest)
    list_pkgs . ./internal/api ./internal/server/... ./internal/tunnel/... ./internal/envs/... ./internal/agents
    taken=("${LISTED[@]}")
    list_pkgs . ./...
    pkgs=()
    for p in "${LISTED[@]}"; do
      skip=0
      for t in "${taken[@]}"; do
        [ "${p}" = "${t}" ] && skip=1 && break
      done
      [ "${skip}" -eq 1 ] || pkgs+=("${p}")
    done
    go_passes . "${pkgs[@]}"
    ;;
  agent-web)
    # WR-05: the web results are named stress-web-N, which the clean-up above does
    # not match, so an earlier run's would be reported next to this one's.
    rm -f "${results}/stress-web-"*
    for m in agent proto; do
      list_pkgs "${m}" ./...
      go_passes "${m}" "${LISTED[@]}"
    done
    # D-03: the web bar, ten shuffled runs with seeds printed and kept beside the JSON.
    i=1
    while [ "${i}" -le "${web_runs}" ]; do
      left="$(seconds_left)"
      if [ "${left}" -le 0 ]; then
        echo "stress: ${shard} budget of ${budget_minutes} minutes used up before web run ${i}" >&2
        failed=1
        i=$((i + 1))
        continue
      fi
      echo "stress: web run ${i}/${web_runs}, seed ${i} (reproduce with make test-web WEB_SEED=${i}; ${left}s of the ${budget} budget left)"
      echo "${i}" >"${results}/stress-web-${i}.seed"
      timeout --kill-after=30s "${left}s" pnpm -C web exec vitest run --sequence.shuffle "--sequence.seed=${i}" --reporter=default --reporter=json "--outputFile.json=${results}/stress-web-${i}.json"
      rc=$?
      if [ "${rc}" -eq 124 ] || [ "${rc}" -eq 137 ]; then
        echo "stress: web run ${i} (seed ${i}) exceeded the remaining budget (${budget}) and was stopped" >&2
        failed=1
      elif [ "${rc}" -ne 0 ]; then
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
