#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Keeps D-15 true: CI never retries a test, a check or a job. A run that went red
# is a defect to fix, not noise to try again. The gate fails on every form of
# retry in the files CI runs:
#   - gotestsum --rerun-fails
#   - a Playwright or Vitest retries/retry value other than 0
#   - curl --retry (and --retry-all-errors, --retry-delay, ...)
#   - a workflow step that uses a retry action
#   - a workflow step that reruns a run (gh run rerun)
# Not a retry: Docker's --health-retries flag, and retries: under a compose
# healthcheck: key. Those are readiness probes of a container, not a second try
# of a test that failed.
#
# Usage: scripts/no-retries-check.sh [PATH...]
# With no paths it scans Makefile, scripts/*.sh (not this gate or its tests), the workflows,
# the Playwright configs and the Vitest config. Full-line comments are skipped.
# One line per finding: no-retries-check: <file>:<line>: <what> (D-15 ...)
set -euo pipefail

cd "$(dirname "$0")/.."

if [ "$#" -gt 0 ]; then
  files=("$@")
else
  files=(Makefile)
  for f in scripts/*.sh .github/workflows/*.yml .github/workflows/*.yaml e2e/*.config.ts web/vitest.config.ts; do
    [ -f "$f" ] || continue
    # This gate and its fail-first tests name every retry form on purpose.
    case "$f" in scripts/no-retries-check.sh | scripts/test-scripts.sh) continue ;; esac
    files+=("$f")
  done
fi

status=0
for f in "${files[@]}"; do
  [ -f "$f" ] || { echo "no-retries-check: ${f}: no such file" >&2; status=1; continue; }
  out="$(awk -v file="$f" '
    function indent_of(s,   m) { match(s, /^[ \t]*/); return RLENGTH }
    function report(n, what) {
      printf "no-retries-check: %s:%d: %s (D-15: CI never retries; fix the cause)\n", file, n, what
    }
    { lines[NR] = $0 }
    END {
      for (n = 1; n <= NR; n++) {
        line = lines[n]
        if (line ~ /^[ \t]*(#|\/\/)/) continue
        if (line ~ /--rerun-fails/) report(n, "gotestsum --rerun-fails reruns failed tests")
        if (line ~ /(^|[ \t=])--retry([ \t=-]|$)/) report(n, "curl --retry retries a failed request")
        if (line ~ /gh[ \t]+run[ \t]+rerun/) report(n, "gh run rerun reruns a run")
        if (line ~ /^[ \t-]*uses:.*[Rr]etry/) report(n, "a retry action reruns a step")
        if (match(line, /(^|[^A-Za-z_-])(retries|retry)[ \t]*[:=][ \t]*/)) {
          rest = substr(line, RSTART + RLENGTH)
          key = substr(line, RSTART, RLENGTH)
          if (rest ~ /^0([ \t,;}]|$)/) continue
          if (key ~ /retry/ && rest == "") continue
          # a compose healthcheck probes a container: exempt by its parent key
          if (file ~ /\.ya?ml$/ && key ~ /retries/) {
            ind = indent_of(line); parent = ""
            for (m = n - 1; m >= 1; m--) {
              if (lines[m] ~ /^[ \t]*$/ || lines[m] ~ /^[ \t]*#/) continue
              if (indent_of(lines[m]) < ind) { parent = lines[m]; break }
            }
            if (parent ~ /^[ \t-]*healthcheck:/) continue
          }
          report(n, "a retries/retry value other than 0 reruns a failed test")
        }
      }
    }' "$f")"
  if [ -n "${out}" ]; then
    printf '%s\n' "${out}" >&2
    status=1
  fi
done
exit "${status}"
