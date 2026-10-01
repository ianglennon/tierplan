#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Real-binary smoke check: starts bin/bayplan on a scratch port and working
# directory and asserts the walking-skeleton contract. Later plans extend it.
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${SMOKE_PORT:-17480}"
BASE="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d)"
LOG="${WORK}/server.log"
PID=""

cleanup() {
  if [ -n "${PID}" ]; then
    kill "${PID}" 2>/dev/null || true
    wait "${PID}" 2>/dev/null || true
  fi
  rm -rf "${WORK}"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $1" >&2
  if [ -s "${LOG}" ]; then
    echo "--- server log ---" >&2
    cat "${LOG}" >&2
  fi
  exit 1
}

[ -x bin/bayplan ] || fail "bin/bayplan missing; run make build first"

BIN="$(pwd)/bin/bayplan"
(cd "${WORK}" && exec "${BIN}" serve --listen "127.0.0.1:${PORT}") >"${LOG}" 2>&1 &
PID=$!

up=""
for _ in $(seq 1 100); do
  if curl -fsS "${BASE}/api/version" -o "${WORK}/version.json" 2>/dev/null; then
    up=1
    break
  fi
  kill -0 "${PID}" 2>/dev/null || fail "server exited during startup"
  sleep 0.1
done
[ -n "${up}" ] || fail "/api/version did not answer within 10 seconds"

for field in display sourceTreeUrl; do
  grep -q "\"${field}\"" "${WORK}/version.json" || fail "/api/version has no ${field}"
done

code="$(curl -sS -o "${WORK}/login.html" -w '%{http_code}' "${BASE}/login")"
[ "${code}" = "200" ] || fail "/login returned ${code}, want 200"
grep -q 'id="root"' "${WORK}/login.html" || fail "/login is not the SPA (no id=\"root\")"

code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/api/does-not-exist")"
[ "${code}" = "404" ] || fail "/api/does-not-exist returned ${code}, want 404"

code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/docs")"
[ "${code}" = "404" ] || fail "/docs returned ${code}, want 404"

curl -sSI "${BASE}/api/version" | grep -qi '^content-security-policy:' \
  || fail "Content-Security-Policy header missing"

echo "SMOKE OK"
