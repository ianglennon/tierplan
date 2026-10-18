#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# D-22: a hung test must end in Go's goroutine dump naming the test, not in the
# job being killed with nothing to read. So for every CI job, the go test time of
# the make targets the job runs must add up to at least MARGIN (10) minutes below
# the job's `timeout-minutes`. The time is read from `make -n`, so the Makefile
# stays the single place it is written:
#   - a target whose recipe carries the literal TEST_BUDGET=<N>m has that budget as
#     its time (it gives each go test pass the time left in the budget, so the
#     passes of one target cannot add up past it);
#   - otherwise its time is the SUM of every `go test -timeout` (gotestsum lines
#     count too) in the recipe;
#   - a go test run in a shell loop (for/while/until) without a TEST_BUDGET cannot
#     be counted and is refused;
#   - a job's total is the sum over the make targets it runs.
# Also refused, because each is a way for the gate to stop meaning anything: a job
# that runs make targets but has no timeout-minutes (GitHub would allow 360), a
# workflow in which no job is found, and `-timeout 0` (no timeout).
#
# Usage: scripts/timeouts-check.sh [WORKFLOW [MAKEFILE]]
# Defaults: .github/workflows/ci.yml and Makefile, relative to the repository root.
# Prints one line per violation naming the job, the target and both values, and
# exits 1 if there is any. A job with no go test timeout is fine (go's own default
# applies).
set -euo pipefail

MARGIN=10
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKFLOW="${1:-${ROOT}/.github/workflows/ci.yml}"
MAKEFILE="${2:-${ROOT}/Makefile}"
if [ "$#" -eq 0 ]; then cd "${ROOT}"; fi

for f in "${WORKFLOW}" "${MAKEFILE}"; do
  if [ ! -f "${f}" ]; then
    echo "timeouts-check: ${f} does not exist" >&2
    exit 2
  fi
done

# job <TAB> timeout-minutes <TAB> make target, one line per target a job runs.
jobs_targets() {
  awk '
    function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
    /^jobs:/ { in_jobs = 1; next }
    !in_jobs { next }
    /^[^ #]/ { in_jobs = 0; next }
    /^  [A-Za-z0-9_-]+:[ \t]*$/ { job = trim($0); sub(/:$/, "", job); print "JOB\t" job; next }
    /^    timeout-minutes:/ {
      v = $0; sub(/^    timeout-minutes:[ \t]*/, "", v); sub(/[ \t#].*$/, "", v)
      print "LIMIT\t" job "\t" v
      next
    }
    /^[ \t]*#/ { next }
    {
      line = $0
      gsub(/\$\{\{[^}]*\}\}/, "", line)
      nseg = split(line, seg, /&&|;|\|\|/)
      for (i = 1; i <= nseg; i++) {
        s = trim(seg[i])
        sub(/^- /, "", s); sub(/^run:[ \t]*/, "", s); s = trim(s)
        if (s !~ /^make([ \t]|$)/) continue
        nw = split(s, w, /[ \t]+/)
        for (j = 2; j <= nw; j++) {
          if (w[j] == "" || w[j] ~ /^-/ || w[j] ~ /=/ || w[j] ~ /[$"\x27]/) continue
          print job "\t" w[j]
        }
      }
    }
  ' "${WORKFLOW}"
}

# The go test time of a make target, as "<minutes> <kind>" with kind one of
#   budget   the target's TEST_BUDGET=<N>m (the sum, if the plan shows several)
#   timeout  its one -timeout in a go test/gotestsum line (minutes 0 when it has none,
#            and go's own default then applies); timeouts is the SUM of several
#   loop     a go test run in a shell loop with no TEST_BUDGET (cannot be counted)
#   zero     a -timeout of 0, which is no timeout
# A gotestsum line counts too: its -timeout sits after the `--` and goes to go test.
# A make that cannot plan the target is an error (exit 2), never a silent 0.
target_time() {
  local plan logical budget gt
  if ! plan="$(env -u MAKEFLAGS -u MAKELEVEL make -n -f "${MAKEFILE}" "$1" 2>&1)"; then
    echo "timeouts-check: make -n $1 failed:" >&2
    printf '%s\n' "${plan}" | sed 's/^/    /' >&2
    return 2
  fi
  # One line per shell command: make -n prints a recipe line with its backslash
  # continuations, and a loop and its go test sit on different physical lines.
  logical="$(printf '%s\n' "${plan}" | awk '{ if (sub(/\\$/, "")) { buf = buf $0 " "; next } print buf $0; buf = "" } END { if (buf != "") print buf }')"
  budget="$(printf '%s\n' "${logical}" | { grep -oE 'TEST_BUDGET=[0-9]+m' || true; } | awk -F'[=m]' '{ n += $2 } END { print n + 0 }')"
  if [ "${budget}" -gt 0 ]; then
    echo "${budget} budget"
    return 0
  fi
  gt="$(printf '%s\n' "${logical}" |
    { grep -E '(^|[^[:alnum:]_])go( -C [^ ]+)? test([[:space:]]|$)|gotestsum@[^[:space:]]+[[:space:]]' || true; })"
  if printf '%s\n' "${gt}" | grep -qE '(^|[;&|({[:space:]])(for|while|until)[[:space:]][^;]*;[[:space:]]*do([[:space:];]|$)'; then
    echo "0 loop"
    return 0
  fi
  printf '%s\n' "${gt}" |
    { grep -oE -- '-timeout[ =]([0-9]+[hms])+|-timeout[ =]0+([[:space:]]|$)' || true; } |
    awk '
      { v = $0; sub(/^-timeout[ =]/, "", v); m = 0; cnt++
        while (match(v, /^[0-9]+[hms]/)) {
          n = substr(v, 1, RLENGTH - 1) + 0; u = substr(v, RLENGTH, 1)
          m += (u == "h") ? n * 60 : (u == "m") ? n : n / 60
          v = substr(v, RLENGTH + 1)
        }
        if (m == 0) zero = 1
        total += m }
      END { printf "%d %s\n", (total == int(total)) ? total : int(total) + 1, zero ? "zero" : (cnt > 1) ? "timeouts" : "timeout" }
    '
}

declare -A limit seen_job tinfo job_targets
jobs_list=()
while IFS=$'\t' read -r a b c; do
  case "${a}" in
    JOB)
      if [ -z "${seen_job[${b}]:-}" ]; then
        seen_job["${b}"]=1
        jobs_list+=("${b}")
      fi
      ;;
    LIMIT) limit["${b}"]="${c}" ;;
    *)
      # a = job, b = target
      if [ -z "${tinfo[${b}]:-}" ]; then
        tinfo["${b}"]="$(target_time "${b}")" || exit 2
      fi
      case " ${job_targets[${a}]:-} " in
        *" ${b} "*) ;;
        *) job_targets["${a}"]="${job_targets[${a}]:-} ${b}" ;;
      esac
      ;;
  esac
done < <(jobs_targets)

if [ "${#jobs_list[@]}" -eq 0 ]; then
  echo "timeouts-check: found no job in ${WORKFLOW}" >&2
  exit 1
fi

violations=0
violate() {
  echo "timeouts-check: $*"
  violations=$((violations + 1))
}

for job in $(printf '%s\n' "${jobs_list[@]}" | sort); do
  targets="${job_targets[${job}]:-}"
  [ -n "${targets}" ] || continue
  if [ -z "${limit[${job}]:-}" ]; then
    violate "job ${job} runs make${targets} but has no timeout-minutes (GitHub would allow 360 minutes, so a hang ends in a silent kill)"
    continue
  fi
  total=0
  counted=()
  kinds=""
  for t in ${targets}; do
    read -r minutes kind <<<"${tinfo[${t}]}"
    case "${kind}" in
      loop) violate "job ${job}: make ${t} runs go test in a loop; give it a TEST_BUDGET so the passes can be counted" ;;
      zero) violate "job ${job} runs make ${t} with go test -timeout 0, which is no timeout" ;;
    esac
    if [ "${minutes}" -gt 0 ]; then
      total=$((total + minutes))
      counted+=("${t}")
      kinds="${kinds} ${kind}"
    fi
  done
  [ "${total}" -gt 0 ] || continue
  max=$((limit[${job}] - MARGIN))
  if [ "${total}" -gt "${max}" ]; then
    if [ "${#counted[@]}" -eq 1 ] && [ "${kinds# }" = "timeout" ]; then
      what="go test -timeout ${total}m"
    elif [ "${#counted[@]}" -eq 1 ] && [ "${kinds# }" = "budget" ]; then
      what="a TEST_BUDGET of ${total}m"
    else
      what="${total}m of go test time"
    fi
    violate "job ${job} runs make ${counted[*]} with ${what} but the job allows ${limit[${job}]} minutes (needs a ${MARGIN} minute margin)"
  fi
done

if [ "${violations}" -gt 0 ]; then
  exit 1
fi
echo "timeouts-check: OK (${#jobs_list[@]} jobs)"
