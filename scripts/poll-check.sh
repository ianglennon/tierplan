#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Keeps D-09 true: polling in a test is for a condition outside the test's control
# (a real engine, an OS lock, a child process). A condition that code in the same
# process brings about is waited on through a signal, never polled.
#
# A *_test.go file that calls testsupport.WaitFor( or testsupport.Poll( passes when
#   - its //go:build line names integration or swarm (the real-engine suites poll
#     the engine by nature), or
#   - each call carries "// testsupport: external" on its own line or on the line
#     above it.
# Anything else is a finding. Helper packages (servertest, dockertest, ...) are not
# _test.go files and are out of scope. Full-line comments are not calls.
#
# Usage: scripts/poll-check.sh [FILE...]
# With no files it reads every *_test.go the repository tracks or has not yet
# ignored. One line per finding: poll-check: <file>:<line>: <what> (D-09 ...)
set -euo pipefail

cd "$(dirname "$0")/.."

files=()
if [ "$#" -gt 0 ]; then
  files=("$@")
else
  while IFS= read -r -d '' f; do
    [ -f "$f" ] || continue # tracked, but deleted in the working tree (not yet staged)
    files+=("$f")
  done < <(git ls-files -z --cached --others --exclude-standard -- '*_test.go')
fi

status=0
for f in "${files[@]}"; do
  [ -f "$f" ] || { echo "poll-check: ${f}: no such file" >&2; status=1; continue; }
  out="$(awk -v file="$f" '
    { lines[NR] = $0 }
    END {
      tagged = 0
      for (n = 1; n <= NR; n++) {
        if (lines[n] ~ /^\/\/go:build /) {
          expr = lines[n]
          gsub(/![A-Za-z0-9_.]+/, "", expr)          # a negated tag does not select the suite
          if (expr ~ /(^|[^A-Za-z0-9_])(integration|swarm)([^A-Za-z0-9_]|$)/) tagged = 1
        }
        if (n > 40 && !tagged) break                  # constraints precede the package clause
      }
      if (tagged) exit
      for (n = 1; n <= NR; n++) {
        line = lines[n]
        if (line ~ /^[ \t]*\/\//) continue
        if (line !~ /testsupport\.(WaitFor|Poll)\(/) continue
        if (line ~ /\/\/[ \t]*testsupport:[ \t]*external/) continue
        if (n > 1 && lines[n - 1] ~ /^[ \t]*\/\/[ \t]*testsupport:[ \t]*external/) continue
        printf "poll-check: %s:%d: testsupport.WaitFor/Poll in a test that is not integration- or swarm-tagged and has no \"// testsupport: external\" marker (D-09: wait on a signal; poll only for a condition outside the test)\n", file, n
      }
    }' "$f")"
  if [ -n "${out}" ]; then
    printf '%s\n' "${out}" >&2
    status=1
  fi
done
exit "${status}"
