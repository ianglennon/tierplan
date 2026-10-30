#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# Timings report from `go test -json` output (gotestsum's --jsonfile has the same
# format). For every FILE it reads the package-level pass/fail events and prints
# a Markdown table of package wall times (slowest first) with the shuffle seed
# the package printed, then the slowest top-level tests. It is how the speed
# work in phase 6.1 states its before and after numbers the same way.
#
# Usage: scripts/test-timings.sh [--slowest N] [--threshold SECONDS] FILE...
#   --slowest N          list at most N tests (default 20)
#   --threshold SECONDS  list only tests at or above this many seconds (default 1)
# Exit status: 0 on success, 2 for a usage error, 1 when a FILE is missing, is
# not readable JSON lines, or holds no package-level event.
set -euo pipefail

usage() {
  echo "usage: scripts/test-timings.sh [--slowest N] [--threshold SECONDS] FILE..." >&2
  exit 2
}

slowest=20
threshold=1
files=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --slowest)
      [ "$#" -ge 2 ] || usage
      slowest="$2"
      shift 2
      ;;
    --threshold)
      [ "$#" -ge 2 ] || usage
      threshold="$2"
      shift 2
      ;;
    -*) usage ;;
    *)
      files+=("$1")
      shift
      ;;
  esac
done
[ "${#files[@]}" -gt 0 ] || usage
[[ "${slowest}" =~ ^[0-9]+$ ]] || usage
[[ "${threshold}" =~ ^[0-9]+([.][0-9]+)?$ ]] || usage

scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT

# Keep only what the report needs from each file, so a large -race log is not
# held in memory whole. A file that is missing or not JSON lines stops here.
events="${scratch}/events.jsonl"
: >"${events}"
for f in "${files[@]}"; do
  if [ ! -r "${f}" ] || [ -d "${f}" ]; then
    echo "test-timings: cannot read ${f}" >&2
    exit 1
  fi
  part="${scratch}/part.jsonl"
  if ! jq -c 'select(.Action == "pass" or .Action == "fail" or (.Action == "output" and ((.Output // "") | startswith("-test.shuffle "))))
      | {Package, Test, Action, Elapsed, Output}' "${f}" >"${part}" 2>"${scratch}/jq.err"; then
    echo "test-timings: ${f} is not go test -json output (JSON lines): $(head -n 1 "${scratch}/jq.err")" >&2
    exit 1
  fi
  if ! jq -e -s 'any(.[]; (.Action == "pass" or .Action == "fail") and .Test == null and .Package != null)' "${part}" >/dev/null; then
    echo "test-timings: ${f} has no package-level pass or fail event" >&2
    exit 1
  fi
  cat "${part}" >>"${events}"
done

jq -n -r --argjson n "${slowest}" --argjson thr "${threshold}" '
  def r3: (. * 1000 | round) / 1000;
  [inputs] as $ev
  | ($ev | map(select(.Action == "output" and .Test == null))
      | group_by(.Package)
      | map({key: .[0].Package, value: (.[0].Output | capture("^-test[.]shuffle (?<s>[0-9]+)").s)})
      | from_entries) as $seeds
  | ($ev | map(select((.Action == "pass" or .Action == "fail") and .Test == null and .Package != null))
      | sort_by(-(.Elapsed // 0))) as $pkgs
  | ($ev | map(select((.Action == "pass" or .Action == "fail") and .Test != null
                      and (.Test | contains("/") | not) and ((.Elapsed // 0) >= $thr)))
      | sort_by(-(.Elapsed // 0)) | .[:$n]) as $tests
  | "## Package wall times",
    "",
    "| package | seconds | result | shuffle seed |",
    "| --- | ---: | --- | --- |",
    ($pkgs[] | "| \(.Package) | \((.Elapsed // 0) | r3) | \(.Action) | \($seeds[.Package] // "") |"),
    "",
    "Total: \($pkgs | length) packages, \(($pkgs | map(.Elapsed // 0) | add) | r3) s summed over packages (packages run in parallel, so this is not elapsed time)",
    "",
    "## Slowest tests",
    "",
    "| test | package | seconds |",
    "| --- | --- | ---: |",
    ($tests[] | "| \(.Test) | \(.Package) | \((.Elapsed // 0) | r3) |")
' "${events}"
