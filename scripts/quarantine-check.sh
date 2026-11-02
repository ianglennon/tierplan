#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Keeps D-12 and D-13 true: a quarantined test is never silent, and the phase exits
# with none. The call sites of testsupport.Quarantine( in *_test.go files must match
# the rows of the "## Quarantined tests" table in the doc, in both directions:
#   - every call is listed with its package, its test name and the date its reason
#     starts with (the reason of a call is "YYYY-MM-DD <tracking ref> ..."), and
#   - every row is backed by a call in that package and test, with a reason text.
# The test of a call is the top-level func that encloses it.
#
# Usage: scripts/quarantine-check.sh [--none] [DOC]
#   DOC defaults to docs/testing.md. With --none any call site at all is a finding,
#   which is what the phase exit gate uses (D-13).
# Like poll-check.sh it reads the *_test.go files the repository tracks or has not
# yet ignored. One line per finding: quarantine-check: <file>:<line>: <what>
set -euo pipefail

cd "$(dirname "$0")/.."

none=0
doc=""
for arg in "$@"; do
  case "${arg}" in
    --none) none=1 ;;
    -*) echo "usage: scripts/quarantine-check.sh [--none] [DOC]" >&2; exit 2 ;;
    *) doc="${arg}" ;;
  esac
done
doc="${doc:-docs/testing.md}"
[ -f "${doc}" ] || { echo "quarantine-check: ${doc}: no such file" >&2; exit 1; }

# Call sites: file|line|package dir|test|date
calls="$(mktemp)"
rows="$(mktemp)"
trap 'rm -f "${calls}" "${rows}"' EXIT

while IFS= read -r -d '' f; do
  [ -f "${f}" ] || continue # tracked, but deleted in the working tree (not yet staged)
  awk -v file="${f}" -v pkg="$(dirname "${f}")" '
    /^func / {
      name = $0
      sub(/^func[ \t]+(\([^)]*\)[ \t]*)?/, "", name)
      sub(/[^A-Za-z0-9_].*$/, "", name)
      current = name
    }
    /^[ \t]*\/\// { next }
    /testsupport\.Quarantine\(/ {
      date = "-"
      if (match($0, /[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]/)) date = substr($0, RSTART, RLENGTH)
      printf "%s|%d|%s|%s|%s\n", file, NR, pkg, (current == "" ? "-" : current), date
    }' "${f}" >>"${calls}"
done < <(git ls-files -z --cached --others --exclude-standard -- '*_test.go')

# Table rows: line|test|package|since|reason, between the heading
# and the next heading; the header and separator rows are not entries.
awk '
  /^## Quarantined tests[ \t]*$/ { in_table = 1; next }
  /^## / { in_table = 0 }
  in_table && /^[ \t]*\|/ {
    line = $0
    gsub(/^[ \t]*\|/, "", line)
    sub(/\|[ \t]*$/, "", line)
    n = split(line, cell, "|")
    for (i = 1; i <= n; i++) { gsub(/^[ \t`]+|[ \t`]+$/, "", cell[i]) }
    if (cell[1] == "Test" || cell[1] ~ /^:?-+:?$/) next
    printf "%d|%s|%s|%s|%s\n", NR, cell[1], cell[2], cell[3], cell[4]
  }' "${doc}" >"${rows}"

US=$'\037'
status=0
finding() { echo "quarantine-check: $*" >&2; status=1; }

while IFS='|' read -r file line pkg test date; do
  [ -n "${file}" ] || continue
  if [ "${none}" -eq 1 ]; then
    finding "${file}:${line}: ${test} in ${pkg} is quarantined, and this check allows none (D-13: fix or delete it)"
    continue
  fi
  match=""
  while IFS='|' read -r rline rtest rpkg rsince rreason; do
    if [ "${rtest}" = "${test}" ] && [ "${rpkg}" = "${pkg}" ]; then match="${rline}${US}${rsince}${US}${rreason}"; break; fi
  done <"${rows}"
  if [ -z "${match}" ]; then
    finding "${file}:${line}: ${test} in ${pkg} calls testsupport.Quarantine but is not in the \"Quarantined tests\" table of ${doc} (D-12: list it with its date and reason)"
    continue
  fi
  IFS="${US}" read -r _ since reason <<<"${match}"
  if [ "${date}" = "-" ] || [ "${since}" != "${date}" ]; then
    finding "${file}:${line}: ${test} in ${pkg} is quarantined with the date ${date} but ${doc} lists ${since:-no date} (D-12: they must agree)"
  elif [ -z "${reason}" ]; then
    finding "${doc}: ${test} in ${pkg} is listed without a reason (D-12)"
  fi
done <"${calls}"

if [ "${none}" -eq 0 ]; then
  while IFS='|' read -r rline rtest rpkg _rest; do
    [ -n "${rtest}" ] || continue
    backed=0
    while IFS='|' read -r _f _l cpkg ctest _d; do
      if [ "${ctest}" = "${rtest}" ] && [ "${cpkg}" = "${rpkg}" ]; then backed=1; break; fi
    done <"${calls}"
    if [ "${backed}" -eq 0 ]; then
      finding "${doc}:${rline}: ${rtest} in ${rpkg} is listed but no testsupport.Quarantine call backs it (D-12: remove the row when the test is fixed)"
    fi
  done <"${rows}"
fi
exit "${status}"
