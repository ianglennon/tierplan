#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# Keeps docs/testing.md "Waiting in tests" true: a test does not wait a fixed, short length
# of real time. A wait, a bound or a clock that follows real time is either a testsupport
# wait (a generous failure deadline) or it says why it cannot fail a correct run.
#
# Go (*_test.go). A line is a finding when it calls time.After(, time.NewTimer(,
# context.WithTimeout(, context.WithDeadline( or time.Since(, or sets a Timeout-named struct
# field to a short duration literal (Timeout: 5 * time.Second), unless
#   - the line uses testsupport. (its deadlines are generous), or
#   - it holds a literal of at least 30 s (N * time.Second with N >= 30, time.Minute,
#     time.Hour), or
#   - it is a t.Log/t.Logf call (a measurement printed for people, nothing asserted), or
#   - it carries "// wall-clock: <reason>" at its end or on the line above, with a reason.
# Full-line comments are not calls. proto/testsupport/ implements the waits and is exempt.
#
# Web (*.test.ts, *.test.tsx). A line is a finding when it
#   - sets shouldAdvanceTime: true (the fake clock then follows real time),
#   - sets timeout: <n> with n below asyncUtilTimeout (web/src/test-setup.ts),
#   - ends a test with }, <n>) where n is below testTimeout (web/vitest.config.ts), or
#   - calls vi.waitFor( with no timeout (Vitest then waits 1 s),
# unless it carries "// wall-clock: <reason>" at its end or on the line above. Both
# thresholds are read from those files; one that cannot be read is an error (exit 2).
#
# What it cannot see: a short duration held in a variable or parameter and used later as a
# deadline (a "within" argument, "Timeout: timeout"), because the value is not on the line
# that uses it. docs/testing.md says how to keep those out; review catches the rest.
#
# Usage: scripts/wallclock-check.sh [FILE...]
# With no files it reads every *_test.go and web *.test.ts(x) the repository tracks or has
# not yet ignored (a tracked file deleted from the working tree is skipped). A name that
# contains _test.go gets the Go rule and one that contains .test.ts the web rule, so the
# .txt fixtures work. One line per finding:
#   wallclock-check: <file>:<line>: <what> (docs/testing.md "Waiting in tests")
# Exit 1 on a finding, 2 on a usage or configuration error. WALLCLOCK_VITEST_CONFIG and
# WALLCLOCK_TEST_SETUP name the two web threshold files (the fail-first cases use them).
set -euo pipefail

cd "$(dirname "$0")/.."

vitest_config="${WALLCLOCK_VITEST_CONFIG:-web/vitest.config.ts}"
test_setup="${WALLCLOCK_TEST_SETUP:-web/src/test-setup.ts}"

files=()
explicit=0
if [ "$#" -gt 0 ]; then
  explicit=1
  files=("$@")
else
  while IFS= read -r -d '' f; do
    files+=("$f")
  done < <(git ls-files -z --cached --others --exclude-standard -- '*_test.go' '*.test.ts' '*.test.tsx')
fi

# threshold <file> <key>: the number after "key:" on a non-comment line, underscores removed.
threshold() {
  local file="$1" key="$2" v
  if [ ! -f "${file}" ]; then
    echo "wallclock-check: ${file}: no such file (it sets ${key}, which the web rule needs)" >&2
    exit 2
  fi
  v="$(grep -vE '^[[:space:]]*(//|\*)' "${file}" | grep -oE "${key}:[[:space:]]*[0-9][0-9_]*" | head -n 1 | grep -oE '[0-9][0-9_]*$' | tr -d '_' || true)"
  if [ -z "${v}" ]; then
    echo "wallclock-check: ${file}: no ${key} value found (the web rule needs it)" >&2
    exit 2
  fi
  printf '%s' "${v}"
}

async_ms=""
test_ms=""

read -r -d '' go_awk <<'AWK' || true
function marker(s) { return s ~ /(\/\/|\/\*)[ \t]*wall-clock:[ \t]*[^ \t*\/]/ }
function marked(n) { return marker(lines[n]) || (n > 1 && marker(lines[n - 1])) }
# dur_ok: the text holds a deadline of at least 30 s, or goes through testsupport.
function dur_ok(s,    rest, n) {
  if (s ~ /testsupport\./) return 1
  if (s ~ /time\.(Minute|Hour)/) return 1
  rest = s
  while (match(rest, /[0-9]+[ \t]*\*[ \t]*time\.Second/)) {
    n = substr(rest, RSTART) + 0
    if (n >= 30) return 1
    rest = substr(rest, RSTART + RLENGTH)
  }
  return 0
}
function report(n, what) {
  printf "wallclock-check: %s:%d: %s (docs/testing.md \"Waiting in tests\")\n", file, n, what
}
{ lines[NR] = $0 }
END {
  for (n = 1; n <= NR; n++) {
    line = lines[n]
    if (line ~ /^[ \t]*\/\//) continue
    if (line ~ /time\.After\(|time\.NewTimer\(|context\.WithTimeout\(|context\.WithDeadline\(|time\.Since\(/) {
      if (dur_ok(line) || line ~ /t\.Logf?\(/ || marked(n)) continue
      report(n, "a short real-time wait or bound in a test with no testsupport deadline and no \"// wall-clock: <reason>\" marker")
      continue
    }
    rest = line
    while (match(rest, /[A-Za-z_]*Timeout:[ \t]*/)) {
      value = substr(rest, RSTART + RLENGTH)
      rest = value
      sub(/[,}].*$/, "", value)
      if (value ~ /time\.(Nanosecond|Microsecond|Millisecond|Second)/ && !dur_ok(value) && !marked(n)) {
        report(n, "a Timeout field set to a short duration with no \"// wall-clock: <reason>\" marker")
        break
      }
    }
  }
}
AWK

read -r -d '' web_awk <<'AWK' || true
function marker(s) { return s ~ /(\/\/|\/\*)[ \t]*wall-clock:[ \t]*[^ \t*\/]/ }
function marked(n) { return marker(lines[n]) || (n > 1 && marker(lines[n - 1])) }
function num(s) { gsub(/_/, "", s); return s + 0 }
function report(n, what) {
  printf "wallclock-check: %s:%d: %s (docs/testing.md \"Waiting in tests\")\n", file, n, what
}
# call_args: the text of the call whose "(" is at column col of line n, up to its matching ")".
function call_args(n, col,    depth, text, i, c, m, s) {
  depth = 0; text = ""
  s = substr(lines[n], col)
  for (m = n; m <= NR && m < n + 80; m++) {
    for (i = 1; i <= length(s); i++) {
      c = substr(s, i, 1)
      if (c == "(") depth++
      else if (c == ")") { depth--; if (depth == 0) return text }
      text = text c
    }
    text = text "\n"
    s = lines[m + 1]
  }
  return text
}
{ lines[NR] = $0 }
END {
  for (n = 1; n <= NR; n++) {
    line = lines[n]
    if (line ~ /^[ \t]*(\/\/|\/\*|\*)/) continue
    if (marked(n)) continue
    if (line ~ /shouldAdvanceTime:[ \t]*true/) {
      report(n, "shouldAdvanceTime: true makes the fake clock follow real time; add a \"// wall-clock: <reason>\" marker or advance the clock by hand")
    }
    if (match(line, /(^|[^A-Za-z0-9_])timeout:[ \t]*[0-9][0-9_]*/)) {
      v = substr(line, RSTART, RLENGTH); sub(/^[^t]*timeout:[ \t]*/, "", v)
      if (num(v) < async_ms) report(n, "timeout: " v " is below the " async_ms " ms async deadline (web/src/test-setup.ts); use the default or add a \"// wall-clock: <reason>\" marker")
    }
    if (match(line, /^[ \t]*\}[ \t]*,[ \t]*[0-9][0-9_]*[ \t]*\)/)) {
      v = substr(line, RSTART, RLENGTH); sub(/^[ \t]*\}[ \t]*,[ \t]*/, "", v); sub(/[ \t]*\)$/, "", v)
      if (num(v) < test_ms) report(n, "a test timeout of " v " ms is below testTimeout " test_ms " ms (web/vitest.config.ts); drop it or add a \"// wall-clock: <reason>\" marker")
    }
    if (match(line, /vi\.waitFor\(/)) {
      args = call_args(n, RSTART + RLENGTH - 1)
      if (args !~ /timeout/ && args !~ /,[ \t\n]*[0-9]/) {
        report(n, "vi.waitFor( with no timeout waits Vitest's own 1 s; pass a timeout or use the waitUnderFakeClock helper (web/src/test-settle.ts)")
      }
    }
  }
}
AWK

status=0
for f in "${files[@]}"; do
  if [ ! -f "${f}" ]; then
    if [ "${explicit}" -eq 1 ]; then
      echo "wallclock-check: ${f}: no such file" >&2
      status=1
    fi
    continue
  fi
  case "${f}" in
    proto/testsupport/*) continue ;;
  esac
  case "${f}" in
    *_test.go*)
      out="$(awk -v file="${f}" "${go_awk}" "${f}")"
      ;;
    *.test.ts*)
      if [ -z "${async_ms}" ]; then async_ms="$(threshold "${test_setup}" asyncUtilTimeout)"; fi
      if [ -z "${test_ms}" ]; then test_ms="$(threshold "${vitest_config}" testTimeout)"; fi
      out="$(awk -v file="${f}" -v async_ms="${async_ms}" -v test_ms="${test_ms}" "${web_awk}" "${f}")"
      ;;
    *)
      echo "wallclock-check: ${f}: not a Go (_test.go) or web (.test.ts, .test.tsx) test file" >&2
      exit 2
      ;;
  esac
  if [ -n "${out}" ]; then
    printf '%s\n' "${out}" >&2
    status=1
  fi
done
exit "${status}"
