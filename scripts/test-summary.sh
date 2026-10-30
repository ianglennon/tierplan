#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# D-21: a red run must say what failed. This reads the results files a test run
# leaves in .devdata/test/ and lists the failing tests, the hung tests and the
# build failures, each with its package and the shuffle seed that reproduces it:
#
#   - go test -json files (gotestsum --jsonfile writes this format). A test that
#     hangs until `go test -timeout` gets no fail event of its own; only the
#     panic's "running tests:" lines name it, so those are read too.
#   - Vitest --reporter=json files. The seed is read from a sibling FILE.seed
#     (make test-web writes it), because the JSON does not carry it.
#
#   - Playwright --reporter=json files (the e2e job writes e2e.json and
#     e2e-copy.json). Each failing, timed-out or interrupted test is named as
#     FILE > describe > spec title [project], and a run-level error (the
#     globalTimeout message, a config failure) gets a line of its own.
#
# The list goes to stdout (the job log) and, when GITHUB_STEP_SUMMARY is set, to
# that file as Markdown. Test names and output are data: they are only ever read
# by jq, control characters are replaced, so a name cannot start a new log line
# (and so cannot forge a workflow command), and Markdown is escaped before it
# reaches the summary. Nothing here is passed to a shell or an expression.
#
# A test that ran more than once in a file (go test -count=N, the stress shards of
# make test-stress) is named with how often it failed: "failed 2/20" (D-16).
#
# Usage: scripts/test-summary.sh FILE...
# Exit: 0 when every file was read, with or without failures (the job's own
# pass or fail comes from the test step); 1 when a file is missing, empty or
# is none of these formats.
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: scripts/test-summary.sh FILE..." >&2
  exit 2
fi

# Records out of a go test -json stream: kind (test|hung|build|package), pkg,
# test, seed, detail.
GO_RECORDS='
def txt: [.[] | select(.Action == "output") | (.Output // "")] | join("");
[.[] | select(type == "object" and has("Action"))] as $ev
| ($ev | map(select(.Package != null)) | group_by(.Package)) as $pkgs
| [ $pkgs[]
    | . as $e
    | $e[0].Package as $p
    | ($e | txt) as $text
    | ([$e[] | select(.Action == "output") | (.Output // "")
        | capture("^-test\\.shuffle (?<s>[0-9]+)\\s*$")] | (.[0].s // "")) as $seed
    | ([$e[] | select(.Action == "fail" and ((.Test // "") != "")) | .Test] | unique) as $failed
    | (if ($text | test("panic: test timed out"))
       then [ $text
              | capture("panic: test timed out after (?<d>[^\\n]*)\\n\\trunning tests:\\n(?<b>(?:\\t\\t[^\\n]*\\n)+)")?
              | .b | scan("\\t\\t(\\S+) \\(([^)]*)\\)") ]
       else [] end) as $hung
    | ($e | any(.Action == "fail" and ((.Test // "") == ""))) as $pkgfail
    | ([$e[] | select(.Action == "fail" and ((.Test // "") == "")) | .FailedBuild // empty] | .[0]) as $fb
    | (($fb != null) or ($text | test("\\[(build|setup) failed\\]"))) as $build
    | ( [ $failed[] | . as $t
          | {kind: "test", pkg: $p, test: $t, seed: $seed, detail: "",
             k: ([$e[] | select(.Action == "fail" and (.Test // "") == $t)] | length),
             n: ([$e[] | select((.Action == "fail" or .Action == "pass") and (.Test // "") == $t)] | length)} ]
      + [ $hung[] | {kind: "hung", pkg: $p, test: .[0], seed: $seed, detail: ("still running after " + .[1] + " when go test timed out")} ]
      + (if $pkgfail and $build then
           [ {kind: "build", pkg: $p, test: "", seed: "",
              detail: ( [ $ev[] | select(.Action == "build-output" and $fb != null and .ImportPath == $fb)
                          | (.Output // "") | split("\n")[] | select(length > 0 and (startswith("#") | not)) ]
                        | .[0:3] | join(" / ") )} ]
         elif $pkgfail and ($failed | length) == 0 and ($hung | length) == 0 then
           [ {kind: "package", pkg: $p, test: "", seed: $seed, detail: "the package failed without a failing test (a panic or exit outside any test, or a TestMain failure)"} ]
         else [] end) )
  ] | add // []
'

# Records out of a Vitest JSON report: one per failed assertion, plus one per
# test file that failed to run at all.
VITEST_RECORDS='
def rel: sub("^.*/web/"; "web/");
def first_line: (. // "") | split("\n")[0];
[ .[0].testResults[]
  | . as $f
  | ( [ ($f.assertionResults // [])[] | select(.status == "failed")
        | {kind: "test", pkg: ($f.name | rel), test: ("> " + (.fullName // .title // "?")),
           seed: $seed, detail: ((.failureMessages // [])[0] | first_line)} ]
    + (if $f.status == "failed" and (($f.assertionResults // []) | length) == 0
       then [ {kind: "package", pkg: ($f.name | rel), test: "", seed: $seed,
               detail: ("the test file failed to run: " + ($f.message | first_line))} ]
       else [] end) )
] | add // []
'

# Records out of a Playwright JSON report. The top-level suites are files; nested
# suites are describe blocks. A test counts as failing when any of its results
# failed, timed out or was interrupted (retries are 0, so there is one result,
# but a retry added later must not hide the failure).
PLAYWRIGHT_RECORDS='
def ansi: gsub("\u001b\\[[0-9;]*[A-Za-z]"; "");
def first_line: (. // "") | ansi | split("\n")[0];
def walk($file; $titles):
  ( (.specs // [])[] as $sp
    | ($sp.tests // [])[] as $t
    | [ ($t.results // [])[] | select(.status == "failed" or .status == "timedOut" or .status == "interrupted") ] as $bad
    | select(($bad | length) > 0)
    | {kind: "test", pkg: $file,
       test: ("> " + (($titles + [$sp.title]) | join(" > ")) + " [" + ($t.projectName // "?") + "]"),
       seed: "",
       detail: (($bad[0].error.message // $bad[0].errors[0].message // $bad[0].status) | first_line)} ),
  ( (.suites // [])[] | walk($file; $titles + [.title]) );
[ (.[0].suites // [])[] | walk(.file; []) ]
+ [ (.[0].errors // [])[] | {kind: "package", pkg: "playwright", test: "", seed: "",
      detail: ("the run reported an error: " + ((.message // "unknown") | first_line))} ]
'

CLEAN='gsub("[\u0001-\u001f\u007f]"; " ") | .[0:300]'

# One line of text per record. Fixed prefix first, so no test name can open a line.
TEXT_LINES='
def clean: tostring | '"${CLEAN}"';
.[]
| (if .seed != "" then " (shuffle seed " + .seed + ")" else "" end) as $s
| (if .detail != "" then ": " + (.detail | clean) else "" end) as $d
| (if (.n // 0) > 1 then " failed " + (.k | tostring) + "/" + (.n | tostring) else "" end) as $r
| if .kind == "test" then "FAIL " + (.pkg | clean) + (if .test != "" then " " + (.test | clean) else "" end) + $s + $r + $d
  elif .kind == "hung" then "HUNG " + (.pkg | clean) + " " + (.test | clean) + $s + $d
  elif .kind == "build" then "BUILD " + (.pkg | clean) + " [build failed]" + $d
  else "PACKAGE " + (.pkg | clean) + $s + $d end
'

MD_LINES='
def md: tostring | '"${CLEAN}"'
  | gsub("&"; "&amp;") | gsub("<"; "&lt;") | gsub(">"; "&gt;")
  | gsub("(?<c>[\\\\`*_\\[\\]()#|!~{}])"; "\\" + .c);
.[]
| (if .seed != "" then " (shuffle seed " + (.seed | md) + ")" else "" end) as $s
| (if .detail != "" then ": " + (.detail | md) else "" end) as $d
| (if (.n // 0) > 1 then " failed " + (.k | tostring) + "/" + (.n | tostring) else "" end) as $r
| if .kind == "test" then "- **FAIL** " + (.pkg | md) + (if .test != "" then " " + (.test | md) else "" end) + $s + $r + $d
  elif .kind == "hung" then "- **HUNG** " + (.pkg | md) + " " + (.test | md) + $s + $d
  elif .kind == "build" then "- **BUILD** " + (.pkg | md) + " (build failed)" + $d
  else "- **PACKAGE** " + (.pkg | md) + $s + $d end
'

status=0
for file in "$@"; do
  label="$(basename -- "${file}")"
  if [ ! -f "${file}" ] || [ ! -r "${file}" ] || [ ! -s "${file}" ]; then
    echo "test-summary: ${file} is missing, empty or unreadable" >&2
    status=1
    continue
  fi

  records=""
  # The format comes from the first JSON value only: a Playwright or Vitest report is
  # one object, go test -json is one event per line. jq stops reading after it.
  kind="$(jq -n -r 'first(inputs | if type == "object" then
      (if has("suites") and has("config") then "playwright"
       elif has("testResults") then "vitest"
       else "gotest" end)
    else "gotest" end) // "gotest"' "${file}" 2>/dev/null || true)"
  case "${kind}" in
    playwright)
      records="$(jq -s -c "${PLAYWRIGHT_RECORDS}" "${file}" 2>/dev/null)" || records=""
      ;;
    vitest)
      seed=""
      if [ -r "${file%.json}.seed" ]; then
        seed="$(tr -cd '0-9' <"${file%.json}.seed" | head -c 20)"
      fi
      records="$(jq -s -c --arg seed "${seed}" "${VITEST_RECORDS}" "${file}" 2>/dev/null)" || records=""
      ;;
    *)
      # go test -json: one object per line. A line that is not JSON is skipped, as
      # go test itself can print one; a file with no event at all is not a result
      # (nothing is printed for it, so records stays empty). jq reads the file
      # itself, so no shell variable ever holds the events (IN-02).
      records="$(jq -n -R -c '[inputs | fromjson? | select(type == "object" and has("Action"))]
        | if length == 0 then empty else '"${GO_RECORDS}"' end' "${file}" 2>/dev/null)" || records=""
      ;;
  esac
  if [ -z "${records}" ]; then
    echo "test-summary: ${file} is neither go test -json, a Vitest JSON report nor a Playwright JSON report" >&2
    status=1
    continue
  fi

  n="$(jq 'length' <<<"${records}")"
  if [ "${n}" -eq 0 ]; then
    echo "test-summary: No failing tests (${label})"
  else
    echo "test-summary: ${n} failing (${label})"
    jq -r "${TEXT_LINES}" <<<"${records}" | sed 's/^/  /'
  fi

  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    safe_label="$(printf '%s' "${label}" | tr -c 'A-Za-z0-9._-' '_')"
    {
      if [ "${n}" -eq 0 ]; then
        printf '### No failing tests (%s)\n\n' "${safe_label}"
      else
        printf '### Failing tests (%s)\n\n' "${safe_label}"
        jq -r "${MD_LINES}" <<<"${records}"
        printf '\n'
      fi
    } >>"${GITHUB_STEP_SUMMARY}"
  fi
done

exit "${status}"
