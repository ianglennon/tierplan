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
DATA="${WORK}/data"
(cd "${WORK}" && BAYPLAN_DATA_DIR="${DATA}" exec "${BIN}" serve --listen "127.0.0.1:${PORT}") >"${LOG}" 2>&1 &
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

# Health endpoints (D-86): fixed bodies, no version details.
code="$(curl -sS -o "${WORK}/healthz.txt" -w '%{http_code}' "${BASE}/healthz")"
[ "${code}" = "200" ] || fail "/healthz returned ${code}, want 200"
[ "$(cat "${WORK}/healthz.txt")" = "ok" ] || fail "/healthz body is not exactly 'ok'"

code="$(curl -sS -o "${WORK}/readyz.txt" -w '%{http_code}' "${BASE}/readyz")"
[ "${code}" = "200" ] || fail "/readyz returned ${code}, want 200"
[ "$(cat "${WORK}/readyz.txt")" = "ready" ] || fail "/readyz body is not exactly 'ready'"

[ -d "${DATA}" ] || fail "data directory ${DATA} was not created"
[ -f "${DATA}/bayplan.db" ] || fail "database ${DATA}/bayplan.db was not created"
grep -q '"database ready"' "${LOG}" || fail "first start did not log 'database ready'"
if grep -q '"snapshot"' "${LOG}"; then
  fail "a first start on an empty data directory must not take a snapshot"
fi

# The healthcheck subcommand, as the container HEALTHCHECK runs it.
BAYPLAN_LISTEN="127.0.0.1:${PORT}" "${BIN}" healthcheck \
  || fail "bayplan healthcheck exited non-zero while the server is up"

# A bad setting stops startup and names the env var (D-11).
if BAYPLAN_PUBLIC_URL=nope BAYPLAN_DATA_DIR="${WORK}/bad" "${BIN}" serve --listen "127.0.0.1:$((PORT + 1))" >"${WORK}/bad.log" 2>&1; then
  fail "serve accepted BAYPLAN_PUBLIC_URL=nope"
fi
grep -q 'BAYPLAN_PUBLIC_URL' "${WORK}/bad.log" || fail "startup error does not name BAYPLAN_PUBLIC_URL"

# After the server stops, the healthcheck must fail.
kill "${PID}"
wait "${PID}" 2>/dev/null || true
PID=""
if BAYPLAN_LISTEN="127.0.0.1:${PORT}" "${BIN}" healthcheck 2>/dev/null; then
  fail "bayplan healthcheck exited 0 after the server stopped"
fi

# Restart on the same data directory (PLAT-01): state is kept, /readyz is 200
# and no migration re-runs, so no pre-migration snapshot is taken (D-83).
LOG="${WORK}/restart.log"
(cd "${WORK}" && BAYPLAN_DATA_DIR="${DATA}" exec "${BIN}" serve --listen "127.0.0.1:${PORT}") >"${LOG}" 2>&1 &
PID=$!
up=""
for _ in $(seq 1 100); do
  if curl -fsS "${BASE}/readyz" -o "${WORK}/readyz2.txt" 2>/dev/null; then
    up=1
    break
  fi
  kill -0 "${PID}" 2>/dev/null || fail "server exited during the restart"
  sleep 0.1
done
[ -n "${up}" ] || fail "/readyz did not answer 200 within 10 seconds after the restart"
[ "$(cat "${WORK}/readyz2.txt")" = "ready" ] || fail "/readyz body after the restart is not exactly 'ready'"
grep -q '"database ready"' "${LOG}" || fail "restart did not log 'database ready'"
if grep -q '"snapshot"' "${LOG}"; then
  fail "the restart on the same data directory re-ran a migration (a snapshot was logged)"
fi
[ ! -d "${DATA}/backups" ] || [ -z "$(ls -A "${DATA}/backups")" ] || fail "the restart left a snapshot in ${DATA}/backups"

echo "SMOKE OK"
