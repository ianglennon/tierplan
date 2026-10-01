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

# Master key (D-13): generated on first start, mode 0600, never logged, and the
# audit chain verifies from a second process against the same data directory.
[ -f "${DATA}/bayplan.key" ] || fail "first start did not generate ${DATA}/bayplan.key"
[ "$(stat -c '%a' "${DATA}/bayplan.key")" = "600" ] || fail "bayplan.key is not mode 600"
grep -q 'back it up' "${LOG}" || fail "first start did not log the key back-up warning"
if grep -qF "$(tr -d '\n' <"${DATA}/bayplan.key")" "${LOG}"; then
  fail "the server log contains the encryption key"
fi
# A count pattern, not "0 entries": later checks write audit entries (setup attempts below).
BAYPLAN_DATA_DIR="${DATA}" "${BIN}" audit verify | grep -Eq '^audit chain OK: [0-9]+ entries$' \
  || fail "bayplan audit verify did not report an intact chain on a fresh data directory"

# Setup gate (D-01, D-05): while no user exists the server mints a token, writes
# it 0600, logs it and `bayplan setup-token` prints the same value.
TOKEN_RE='^[0-9a-f]{8}(-[0-9a-f]{8}){3}$'
[ -f "${DATA}/setup-token" ] || fail "first start did not write ${DATA}/setup-token"
[ "$(stat -c '%a' "${DATA}/setup-token")" = "600" ] || fail "setup-token is not mode 600"
TOKEN="$(BAYPLAN_DATA_DIR="${DATA}" "${BIN}" setup-token)" || fail "bayplan setup-token failed on a fresh data directory"
printf '%s' "${TOKEN}" | grep -Eq "${TOKEN_RE}" || fail "bayplan setup-token printed '${TOKEN}', not a setup token"
grep -q '"setup required"' "${LOG}" || fail "first start did not log 'setup required'"
grep -qF "${TOKEN}" "${LOG}" || fail "the server log does not contain the setup token"

curl -fsS "${BASE}/api/instance" -o "${WORK}/instance.json" || fail "/api/instance did not answer 200"
grep -q '"needsSetup":true' "${WORK}/instance.json" || fail "/api/instance does not report needsSetup true"
code="$(curl -sS -o "${WORK}/setup.html" -w '%{http_code}' "${BASE}/setup")"
[ "${code}" = "200" ] || fail "/setup returned ${code}, want 200 while setup is needed"
grep -q 'id="root"' "${WORK}/setup.html" || fail "/setup is not the SPA"

code="$(curl -sS -o "${WORK}/wrong.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"token":"00000000-00000000-00000000-00000000"}' "${BASE}/api/setup/token")"
[ "${code}" = "401" ] || fail "a wrong setup token returned ${code}, want 401"
grep -q 'invalid_setup_token' "${WORK}/wrong.json" || fail "a wrong setup token did not answer invalid_setup_token"
grep -q 'setup.token_failed' "${LOG}" || fail "a wrong setup token was not audited as setup.token_failed"
if grep -q '00000000-00000000-00000000-00000000' "${LOG}"; then
  fail "the server log contains the attempted setup token"
fi
code="$(curl -sS -o "${WORK}/right.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"token\":\"${TOKEN}\"}" "${BASE}/api/setup/token")"
[ "${code}" = "200" ] || fail "the right setup token returned ${code}, want 200"
grep -q '"defaultInstanceName"' "${WORK}/right.json" || fail "the right setup token did not return the step 3 defaults"
[ "$(BAYPLAN_DATA_DIR="${DATA}" "${BIN}" audit verify)" = "audit chain OK: 1 entries" ] \
  || fail "audit verify does not report exactly the one wrong-token entry"

# Setup completion (D-02, AUTH-01): the token holder creates the one admin, is
# signed in with a hardened cookie, and the token is spent.
PW='correct horse battery'
code="$(curl -sS -o "${WORK}/complete.json" -D "${WORK}/complete.hdr" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"token\":\"${TOKEN}\",\"username\":\"Admin\",\"password\":\"${PW}\",\"instanceName\":\"Smoke lab\",\"publicUrl\":\"https://bayplan.example\"}" \
  "${BASE}/api/setup/complete")"
[ "${code}" = "200" ] || fail "setup completion returned ${code}, want 200: $(cat "${WORK}/complete.json")"
grep -q '"username":"admin"' "${WORK}/complete.json" || fail "setup completion did not return the lower-cased admin"
grep -q '"role":"admin"' "${WORK}/complete.json" || fail "setup completion did not return the admin role"
grep -q '"csrfToken":"[A-Za-z0-9_-]\{20,\}"' "${WORK}/complete.json" || fail "setup completion did not return a CSRF token"
COOKIE_LINE="$(grep -i '^set-cookie:' "${WORK}/complete.hdr" | tr -d '\r')"
printf '%s' "${COOKIE_LINE}" | grep -q '^[Ss]et-[Cc]ookie: __Host-bayplan_session=' || fail "no __Host- session cookie under an https public URL: ${COOKIE_LINE}"
for attr in 'HttpOnly' 'Secure' 'SameSite=Lax' 'Path=/'; do
  printf '%s' "${COOKIE_LINE}" | grep -q "${attr}" || fail "session cookie lacks ${attr}: ${COOKIE_LINE}"
done
if printf '%s' "${COOKIE_LINE}" | grep -qi 'domain='; then fail "session cookie carries a Domain: ${COOKIE_LINE}"; fi
SESSION_ID="$(printf '%s' "${COOKIE_LINE}" | sed 's/^[^=]*=//; s/;.*//')"
grep -qi '^cache-control: .*no-store' "${WORK}/complete.hdr" || fail "setup completion is cacheable"

[ ! -e "${DATA}/setup-token" ] || fail "the setup-token file survived completion"
if BAYPLAN_DATA_DIR="${DATA}" "${BIN}" setup-token >/dev/null 2>&1; then
  fail "bayplan setup-token still prints a token after completion"
fi
code="$(curl -sS -o "${WORK}/replay.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"token\":\"${TOKEN}\",\"username\":\"mallory\",\"password\":\"${PW}\",\"instanceName\":\"x\",\"publicUrl\":\"https://bayplan.example\"}" \
  "${BASE}/api/setup/complete")"
[ "${code}" = "409" ] || fail "a second completion returned ${code}, want 409"
grep -q 'setup_complete' "${WORK}/replay.json" || fail "a second completion did not answer setup_complete"
code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/setup")"
[ "${code}" = "302" ] || fail "/setup returned ${code} after completion, want 302"
curl -fsS "${BASE}/api/instance" -o "${WORK}/instance2.json" || fail "/api/instance did not answer after completion"
grep -q '"needsSetup":false' "${WORK}/instance2.json" || fail "/api/instance still reports needsSetup after completion"
grep -q '"instanceName":"Smoke lab"' "${WORK}/instance2.json" || fail "/api/instance does not report the stored instance name"
# Secrets are never logged, and the database holds only a hash of the session ID.
if grep -qF "${PW}" "${LOG}"; then fail "the server log contains the admin password"; fi
if grep -qF "${SESSION_ID}" "${LOG}"; then fail "the server log contains the session ID"; fi
if grep -qaF "${SESSION_ID}" "${DATA}"/bayplan.db*; then fail "the database contains the raw session ID"; fi
# 3 entries: the wrong token, setup.completed, and the denied replay.
AUDIT_OUT="$(BAYPLAN_DATA_DIR="${DATA}" "${BIN}" audit verify)"
[ "${AUDIT_OUT}" = "audit chain OK: 3 entries" ] || fail "audit verify after setup reports '${AUDIT_OUT}', want 3 entries"
grep -q 'setup.completed' "${LOG}" || fail "setup.completed was not logged"

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
# The admin and the settings survive the restart, and no new setup token is minted.
curl -fsS "${BASE}/api/instance" -o "${WORK}/instance3.json" || fail "/api/instance did not answer after the restart"
grep -q '"needsSetup":false' "${WORK}/instance3.json" || fail "the restart reopened setup"
grep -q '"instanceName":"Smoke lab"' "${WORK}/instance3.json" || fail "the instance name did not survive the restart"
[ ! -e "${DATA}/setup-token" ] || fail "the restart wrote a setup-token although an admin exists"
if grep -q '"snapshot"' "${LOG}"; then
  fail "the restart on the same data directory re-ran a migration (a snapshot was logged)"
fi
[ ! -d "${DATA}/backups" ] || [ -z "$(ls -A "${DATA}/backups")" ] || fail "the restart left a snapshot in ${DATA}/backups"

echo "SMOKE OK"
