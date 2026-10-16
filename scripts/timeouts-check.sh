#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# D-22: a hung test must end in Go's goroutine dump naming the test, not in the
# job being killed with nothing to read. So for every CI job, the largest
# `go test -timeout` of the make targets the job runs must be at least MARGIN
# (10) minutes below the job's `timeout-minutes`. The timeouts are read from
# `make -n`, so the Makefile stays the single place they are written.
#
# Usage: scripts/timeouts-check.sh [WORKFLOW [MAKEFILE]]
# Defaults: .github/workflows/ci.yml and Makefile, relative to the repository root.
# Prints one line per violation naming the job, the target and both values, and
# exits 1 if there is any. A job with no go test timeout is fine.
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
    /^  [A-Za-z0-9_-]+:[ \t]*$/ { job = trim($0); sub(/:$/, "", job); next }
    /^    timeout-minutes:/ {
      v = $0; sub(/^    timeout-minutes:[ \t]*/, "", v); sub(/[ \t#].*$/, "", v)
      limit[job] = v
      if (!(job in seen)) { seen[job] = 1; order[++n] = job }
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
    END { for (j in limit) print "LIMIT\t" j "\t" limit[j] }
  ' "${WORKFLOW}"
}

# Largest `go test -timeout` of a make target, in minutes (0 when it has none).
# A gotestsum line counts too: its -timeout sits after the `--` and goes to go test.
# A make that cannot plan the target is an error (exit 2), never a silent 0.
target_timeout() {
  local plan
  if ! plan="$(env -u MAKEFLAGS -u MAKELEVEL make -n -f "${MAKEFILE}" "$1" 2>&1)"; then
    echo "timeouts-check: make -n $1 failed:" >&2
    printf '%s\n' "${plan}" | sed 's/^/    /' >&2
    return 2
  fi
  printf '%s\n' "${plan}" |
    { grep -E '(^|[^[:alnum:]_])go( -C [^ ]+)? test([[:space:]]|$)|gotestsum@[^[:space:]]+[[:space:]]' || true; } |
    { grep -oE -- '-timeout[ =][0-9]+[hms]([0-9]+[hms])*' || true; } |
    awk '
      { v = $0; sub(/^-timeout[ =]/, "", v); m = 0
        while (match(v, /^[0-9]+[hms]/)) {
          n = substr(v, 1, RLENGTH - 1) + 0; u = substr(v, RLENGTH, 1)
          m += (u == "h") ? n * 60 : (u == "m") ? n : n / 60
          v = substr(v, RLENGTH + 1)
        }
        if (m > best) best = m }
      END { printf "%d\n", (best == int(best)) ? best : int(best) + 1 }
    '
}

declare -A limit worst worst_target
jobs_list=()
while IFS=$'\t' read -r a b c; do
  if [ "${a}" = "LIMIT" ]; then
    limit["${b}"]="${c}"
    jobs_list+=("${b}")
  else
    # a = job, b = target
    t="$(target_timeout "${b}")" || exit 2
    if [ "${t}" -gt "${worst[${a}]:-0}" ]; then
      worst["${a}"]="${t}"
      worst_target["${a}"]="${b}"
    fi
  fi
done < <(jobs_targets)

violations=0
for job in $(printf '%s\n' "${jobs_list[@]}" | sort); do
  t="${worst[${job}]:-0}"
  [ "${t}" -eq 0 ] && continue
  max=$((limit[${job}] - MARGIN))
  if [ "${t}" -gt "${max}" ]; then
    echo "timeouts-check: job ${job} runs make ${worst_target[${job}]} with go test -timeout ${t}m but the job allows ${limit[${job}]} minutes (needs a ${MARGIN} minute margin)"
    violations=$((violations + 1))
  fi
done

if [ "${violations}" -gt 0 ]; then
  exit 1
fi
echo "timeouts-check: OK (${#jobs_list[@]} jobs)"
