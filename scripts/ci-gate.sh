#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# The exit gate's evidence check for phase 6.1 (D-20, D-18). It reads the attempts of
# one CI run and passes only when the last N attempts (default 5) are all of one head
# commit, each concluded success, and no job in them failed, was cancelled or timed
# out. A skipped job is fine. It prints every job's minutes per attempt, then the D-18
# check per attempt: the longest unit-test job (test, web, lint) against the longest
# Docker-backed leg (integration, swarm, e2e). A unit job longer than the longest
# Docker leg prints "ci-gate: D-18 not met in attempt <n>"; that is a warning, the
# exit status reflects only whether the attempts were green, and the exit gate
# (06.1-25) decides on the warning.
#
# The script only reads: it makes GET requests with `gh api` and never reruns or
# dispatches anything (D-15).
#
# Usage: scripts/ci-gate.sh RUN_ID [N]
#        scripts/ci-gate.sh --from-file FILE [N]
# FILE is a JSON array of {attempt, head_sha, conclusion, jobs: [{name, conclusion,
# started_at, completed_at}]}, the shape the gh path is converted to.
# Environment: GITHUB_REPOSITORY (default ianglennon/coaming).
# Exit: 0 the evidence holds, 1 it does not, 2 usage or API error.
set -euo pipefail

usage() {
  echo "usage: scripts/ci-gate.sh RUN_ID [N] | scripts/ci-gate.sh --from-file FILE [N]" >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
file=""
run_id=""
if [ "$1" = "--from-file" ]; then
  [ "$#" -ge 2 ] || usage
  file="$2"
  shift 2
else
  run_id="$1"
  shift
  case "${run_id}" in '' | *[!0-9]*) echo "ci-gate: RUN_ID must be a number, got '${run_id}'" >&2; usage ;; esac
fi
n="${1:-5}"
[ "$#" -le 1 ] || usage
case "${n}" in '' | *[!0-9]* | 0) echo "ci-gate: N must be a positive number, got '${n}'" >&2; usage ;; esac

repo="${GITHUB_REPOSITORY:-ianglennon/coaming}"

if [ -n "${file}" ]; then
  if [ ! -r "${file}" ]; then
    echo "ci-gate: cannot read ${file}" >&2
    exit 2
  fi
  attempts="$(jq -c '.' "${file}")" || { echo "ci-gate: ${file} is not JSON" >&2; exit 2; }
else
  latest="$(gh api "repos/${repo}/actions/runs/${run_id}" --jq .run_attempt)" || { echo "ci-gate: cannot read run ${run_id}" >&2; exit 2; }
  case "${latest}" in '' | *[!0-9]*) echo "ci-gate: run ${run_id} has no attempt number" >&2; exit 2 ;; esac
  docs=()
  a=1
  while [ "${a}" -le "${latest}" ]; do
    meta="$(gh api "repos/${repo}/actions/runs/${run_id}/attempts/${a}" --jq '{attempt: .run_attempt, head_sha: .head_sha, conclusion: .conclusion}')" ||
      { echo "ci-gate: cannot read attempt ${a} of run ${run_id}" >&2; exit 2; }
    jobs="$(gh api --paginate "repos/${repo}/actions/runs/${run_id}/attempts/${a}/jobs" --jq '.jobs[] | {name, conclusion, started_at, completed_at}' | jq -s -c '.')" ||
      { echo "ci-gate: cannot read the jobs of attempt ${a} of run ${run_id}" >&2; exit 2; }
    docs+=("$(jq -c --argjson jobs "${jobs}" '. + {jobs: $jobs}' <<<"${meta}")")
    a=$((a + 1))
  done
  attempts="$(printf '%s\n' "${docs[@]}" | jq -s -c '.')"
fi

total="$(jq 'length' <<<"${attempts}")"
if [ "${total}" -lt "${n}" ]; then
  echo "ci-gate: FAIL: the run has ${total} attempt(s) but ${n} are needed" >&2
  exit 1
fi

# The last N attempts by attempt number.
window="$(jq -c --argjson n "${n}" 'sort_by(.attempt) | .[-$n:]' <<<"${attempts}")"

# minutes of a job; a job that never started or finished counts 0.
MINUTES='def minutes: if (.started_at // null) == null or (.completed_at // null) == null then 0
  else ([(((.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601)) / 60 * 10 | round / 10), 0] | max) end;'

echo "ci-gate: attempt  job  conclusion  minutes"
jq -r "${MINUTES}"' .[] | .attempt as $a | .jobs[] | "\($a)  \(.name)  \(.conclusion // "unfinished")  \(minutes)"' <<<"${window}" | sed 's/^/ci-gate: /'

status=0

# D-20: one head commit.
shas="$(jq -r '[.[].head_sha] | unique | join(" ")' <<<"${window}")"
if [ "$(jq '[.[].head_sha] | unique | length' <<<"${window}")" -ne 1 ]; then
  echo "ci-gate: FAIL: the last ${n} attempts are not of one head commit (${shas})"
  status=1
fi

# D-20: every attempt and every job green.
bad="$(jq -r '.[] | .attempt as $a
  | (if .conclusion != "success" then "attempt \($a) concluded \(.conclusion // "unfinished")" else empty end),
    (.jobs[] | select((.conclusion // "unfinished") as $c | ["success", "skipped", "neutral"] | index($c) | not)
      | "attempt \($a): job \(.name) concluded \(.conclusion // "unfinished")")' <<<"${window}")"
if [ -n "${bad}" ]; then
  while IFS= read -r line; do
    echo "ci-gate: FAIL: ${line}"
  done <<<"${bad}"
  status=1
fi

# D-18: no unit-test job longer than the longest Docker-backed leg in the same attempt.
jq -r "${MINUTES}"'
  .[] | .attempt as $a
  | ([.jobs[] | select(.name | test("^(test|web|lint)$")) | {name, m: minutes}] | sort_by(.m) | last) as $u
  | ([.jobs[] | select(.name | test("^((integration|swarm) \\(|e2e)")) | {name, m: minutes}] | sort_by(.m) | last) as $d
  | if $u == null or $d == null then "attempt \($a): D-18 cannot be judged (no unit job or no Docker-backed job)"
    else "attempt \($a): longest unit job \($u.name) \($u.m) min, longest Docker-backed leg \($d.name) \($d.m) min"
         + (if $u.m > $d.m then "\nD-18 not met in attempt \($a)" else "" end) end' <<<"${window}" | sed 's/^/ci-gate: /'

if [ "${status}" -eq 0 ]; then
  echo "ci-gate: OK: the last ${n} attempt(s) of head ${shas} are all green"
fi
exit "${status}"
