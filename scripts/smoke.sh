#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Real-binary smoke check: starts bin/tierplan on a scratch port and working
# directory and asserts the walking-skeleton contract. Later plans extend it.
#
# The server listens on 127.0.0.1:0 and the script reads the address the kernel gave
# it from the server's own "tierplan listening" log line, so no port is ever probed
# for and released before the server binds it (two runners share each VM). A restart
# gets a new port. SMOKE_PORT pins one (D-23). It starts no container. Needs jq.
set -euo pipefail

cd "$(dirname "$0")/.."

LISTEN_PORT="${SMOKE_PORT:-0}"
PORT=""
BASE=""
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

[ -x bin/tierplan ] || fail "bin/tierplan missing; run make build first"

# await_listening reads the bound address from the "tierplan listening" line the
# server logs once its accept loop runs, sets PORT and BASE from it, and fails if the
# server exits or says nothing within 10 seconds.
await_listening() {
  local line addr
  for _ in $(seq 1 100); do
    line="$(grep -m1 '"msg":"tierplan listening"' "${LOG}" 2>/dev/null || true)"
    if [ -n "${line}" ]; then
      addr="$(printf '%s' "${line}" | jq -r '.addr')" || fail "the listening line is not JSON: ${line}"
      PORT="${addr##*:}"
      [[ "${PORT}" =~ ^[0-9]+$ ]] || fail "the listening line has no port: ${line}"
      BASE="https://localhost:${PORT}"
      return 0
    fi
    kill -0 "${PID}" 2>/dev/null || fail "$1 exited during startup"
    sleep 0.1
  done
  fail "$1 did not log 'tierplan listening' within 10 seconds"
}

BIN="$(pwd)/bin/tierplan"
DATA="${WORK}/data"

# tcurl is curl over TLS, verifying TierPlan's own certificate: the mirror the server writes under
# its data directory is the only thing trusted, and verification is never switched off (D-10).
tcurl() {
  curl --cacert "${DATA}/tls/server.crt" "$@"
}
(cd "${WORK}" && TIERPLAN_DATA_DIR="${DATA}" exec "${BIN}" serve --listen "127.0.0.1:${LISTEN_PORT}") >"${LOG}" 2>&1 &
PID=$!

await_listening "the server"
up=""
for _ in $(seq 1 100); do
  if tcurl -fsS "${BASE}/api/version" -o "${WORK}/version.json" 2>/dev/null; then
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

code="$(tcurl -sS -o "${WORK}/login.html" -w '%{http_code}' "${BASE}/login")"
[ "${code}" = "200" ] || fail "/login returned ${code}, want 200"
grep -q 'id="root"' "${WORK}/login.html" || fail "/login is not the SPA (no id=\"root\")"

code="$(tcurl -sS -o /dev/null -w '%{http_code}' "${BASE}/api/does-not-exist")"
[ "${code}" = "404" ] || fail "/api/does-not-exist returned ${code}, want 404"

code="$(tcurl -sS -o /dev/null -w '%{http_code}' "${BASE}/docs")"
[ "${code}" = "404" ] || fail "/docs returned ${code}, want 404"

tcurl -sSI "${BASE}/api/version" | grep -qi '^content-security-policy:' \
  || fail "Content-Security-Policy header missing"

# Health endpoints (D-86): fixed bodies, no version details.
code="$(tcurl -sS -o "${WORK}/healthz.txt" -w '%{http_code}' "${BASE}/healthz")"
[ "${code}" = "200" ] || fail "/healthz returned ${code}, want 200"
[ "$(cat "${WORK}/healthz.txt")" = "ok" ] || fail "/healthz body is not exactly 'ok'"

code="$(tcurl -sS -o "${WORK}/readyz.txt" -w '%{http_code}' "${BASE}/readyz")"
[ "${code}" = "200" ] || fail "/readyz returned ${code}, want 200"
[ "$(cat "${WORK}/readyz.txt")" = "ready" ] || fail "/readyz body is not exactly 'ready'"

[ -d "${DATA}" ] || fail "data directory ${DATA} was not created"
[ -f "${DATA}/tierplan.db" ] || fail "database ${DATA}/tierplan.db was not created"
grep -q '"database ready"' "${LOG}" || fail "first start did not log 'database ready'"
if grep -q '"snapshot"' "${LOG}"; then
  fail "a first start on an empty data directory must not take a snapshot"
fi

# Master key (D-13): generated on first start, mode 0600, never logged, and the
# audit chain verifies from a second process against the same data directory.
[ -f "${DATA}/tierplan.key" ] || fail "first start did not generate ${DATA}/tierplan.key"
[ "$(stat -c '%a' "${DATA}/tierplan.key")" = "600" ] || fail "tierplan.key is not mode 600"
grep -q 'back it up' "${LOG}" || fail "first start did not log the key back-up warning"
if grep -qF "$(tr -d '\n' <"${DATA}/tierplan.key")" "${LOG}"; then
  fail "the server log contains the encryption key"
fi
# A count pattern, not "0 entries": later checks write audit entries (setup attempts below).
TIERPLAN_DATA_DIR="${DATA}" "${BIN}" audit verify | grep -Eq '^audit chain OK: [0-9]+ entries$' \
  || fail "tierplan audit verify did not report an intact chain on a fresh data directory"

# Setup gate (D-01, D-05): while no user exists the server mints a token, writes
# it 0600, logs it and `tierplan setup-token` prints the same value.
TOKEN_RE='^[0-9a-f]{8}(-[0-9a-f]{8}){3}$'
[ -f "${DATA}/setup-token" ] || fail "first start did not write ${DATA}/setup-token"
[ "$(stat -c '%a' "${DATA}/setup-token")" = "600" ] || fail "setup-token is not mode 600"
TOKEN="$(TIERPLAN_DATA_DIR="${DATA}" "${BIN}" setup-token)" || fail "tierplan setup-token failed on a fresh data directory"
printf '%s' "${TOKEN}" | grep -Eq "${TOKEN_RE}" || fail "tierplan setup-token printed '${TOKEN}', not a setup token"
grep -q '"setup required"' "${LOG}" || fail "first start did not log 'setup required'"
grep -qF "${TOKEN}" "${LOG}" || fail "the server log does not contain the setup token"

tcurl -fsS "${BASE}/api/instance" -o "${WORK}/instance.json" || fail "/api/instance did not answer 200"
grep -q '"needsSetup":true' "${WORK}/instance.json" || fail "/api/instance does not report needsSetup true"
code="$(tcurl -sS -o "${WORK}/setup.html" -w '%{http_code}' "${BASE}/setup")"
[ "${code}" = "200" ] || fail "/setup returned ${code}, want 200 while setup is needed"
grep -q 'id="root"' "${WORK}/setup.html" || fail "/setup is not the SPA"

code="$(tcurl -sS -o "${WORK}/wrong.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d '{"token":"00000000-00000000-00000000-00000000"}' "${BASE}/api/setup/token")"
[ "${code}" = "401" ] || fail "a wrong setup token returned ${code}, want 401"
grep -q 'invalid_setup_token' "${WORK}/wrong.json" || fail "a wrong setup token did not answer invalid_setup_token"
grep -q 'setup.token_failed' "${LOG}" || fail "a wrong setup token was not audited as setup.token_failed"
if grep -q '00000000-00000000-00000000-00000000' "${LOG}"; then
  fail "the server log contains the attempted setup token"
fi
code="$(tcurl -sS -o "${WORK}/right.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"token\":\"${TOKEN}\"}" "${BASE}/api/setup/token")"
[ "${code}" = "200" ] || fail "the right setup token returned ${code}, want 200"
grep -q '"defaultInstanceName"' "${WORK}/right.json" || fail "the right setup token did not return the step 3 defaults"
[ "$(TIERPLAN_DATA_DIR="${DATA}" "${BIN}" audit verify)" = "audit chain OK: 1 entries" ] \
  || fail "audit verify does not report exactly the one wrong-token entry"

# Setup completion (D-02, AUTH-01): the token holder creates the one admin, is
# signed in with a hardened cookie, and the token is spent.
PW='correct horse battery'
code="$(tcurl -sS -o "${WORK}/complete.json" -D "${WORK}/complete.hdr" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"token\":\"${TOKEN}\",\"username\":\"Admin\",\"password\":\"${PW}\",\"instanceName\":\"Smoke lab\",\"publicUrl\":\"https://tierplan.example\"}" \
  "${BASE}/api/setup/complete")"
[ "${code}" = "200" ] || fail "setup completion returned ${code}, want 200: $(cat "${WORK}/complete.json")"
grep -q '"username":"admin"' "${WORK}/complete.json" || fail "setup completion did not return the lower-cased admin"
grep -q '"instanceAdmin":true' "${WORK}/complete.json" || fail "setup completion did not return an instance admin"
grep -q '"csrfToken":"[A-Za-z0-9_-]\{20,\}"' "${WORK}/complete.json" || fail "setup completion did not return a CSRF token"
COOKIE_LINE="$(grep -i '^set-cookie:' "${WORK}/complete.hdr" | tr -d '\r')"
printf '%s' "${COOKIE_LINE}" | grep -q '^[Ss]et-[Cc]ookie: __Host-tierplan_session=' || fail "no __Host- session cookie under an https public URL: ${COOKIE_LINE}"
for attr in 'HttpOnly' 'Secure' 'SameSite=Lax' 'Path=/'; do
  printf '%s' "${COOKIE_LINE}" | grep -q "${attr}" || fail "session cookie lacks ${attr}: ${COOKIE_LINE}"
done
if printf '%s' "${COOKIE_LINE}" | grep -qi 'domain='; then fail "session cookie carries a Domain: ${COOKIE_LINE}"; fi
SESSION_ID="$(printf '%s' "${COOKIE_LINE}" | sed 's/^[^=]*=//; s/;.*//')"
grep -qi '^cache-control: .*no-store' "${WORK}/complete.hdr" || fail "setup completion is cacheable"

[ ! -e "${DATA}/setup-token" ] || fail "the setup-token file survived completion"
if TIERPLAN_DATA_DIR="${DATA}" "${BIN}" setup-token >/dev/null 2>&1; then
  fail "tierplan setup-token still prints a token after completion"
fi
code="$(tcurl -sS -o "${WORK}/replay.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  -d "{\"token\":\"${TOKEN}\",\"username\":\"mallory\",\"password\":\"${PW}\",\"instanceName\":\"x\",\"publicUrl\":\"https://tierplan.example\"}" \
  "${BASE}/api/setup/complete")"
[ "${code}" = "409" ] || fail "a second completion returned ${code}, want 409"
grep -q 'setup_complete' "${WORK}/replay.json" || fail "a second completion did not answer setup_complete"
code="$(tcurl -sS -o /dev/null -w '%{http_code}' "${BASE}/setup")"
[ "${code}" = "302" ] || fail "/setup returned ${code} after completion, want 302"
tcurl -fsS "${BASE}/api/instance" -o "${WORK}/instance2.json" || fail "/api/instance did not answer after completion"
grep -q '"needsSetup":false' "${WORK}/instance2.json" || fail "/api/instance still reports needsSetup after completion"
grep -q '"instanceName":"Smoke lab"' "${WORK}/instance2.json" || fail "/api/instance does not report the stored instance name"
# Secrets are never logged, and the database holds only a hash of the session ID.
if grep -qF "${PW}" "${LOG}"; then fail "the server log contains the admin password"; fi
if grep -qF "${SESSION_ID}" "${LOG}"; then fail "the server log contains the session ID"; fi
if grep -qaF "${SESSION_ID}" "${DATA}"/tierplan.db*; then fail "the database contains the raw session ID"; fi
# 3 entries: the wrong token, setup.completed, and the denied replay.
AUDIT_OUT="$(TIERPLAN_DATA_DIR="${DATA}" "${BIN}" audit verify)"
[ "${AUDIT_OUT}" = "audit chain OK: 3 entries" ] || fail "audit verify after setup reports '${AUDIT_OUT}', want 3 entries"
grep -q 'setup.completed' "${LOG}" || fail "setup.completed was not logged"

# Sign-in and sign-out (AUTH-02, AUTH-03, D-29 to D-33, D-36). The public URL is
# https, so the Origin guard wants it on every mutation and the cookie is __Host-.
ORIGIN='https://tierplan.example'
JSON_H='Content-Type: application/json'
login() { # $1 = username, $2 = password, $3 = optional extra curl args; body in login.json, headers in login.hdr
  tcurl -sS -o "${WORK}/login.json" -D "${WORK}/login.hdr" -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${ORIGIN}" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}" "${BASE}/api/auth/login"
}
code="$(tcurl -sS -o /dev/null -w '%{http_code}' "${BASE}/api/me")"
[ "${code}" = "401" ] || fail "/api/me without a session returned ${code}, want 401"
code="$(tcurl -sS -o "${WORK}/me0.json" -w '%{http_code}' -H "Cookie: __Host-tierplan_session=${SESSION_ID}" "${BASE}/api/me")"
[ "${code}" = "200" ] || fail "/api/me with the setup session returned ${code}, want 200: $(cat "${WORK}/me0.json")"
grep -q '"username":"admin"' "${WORK}/me0.json" || fail "/api/me did not return the admin"
grep -q '"environments":{"\*":\[' "${WORK}/me0.json" || fail "/api/me did not list the admin's all-environment permissions"

# Every failure is the same 401 with the same body: wrong password and unknown user.
code="$(login admin 'definitely not the password')"
[ "${code}" = "401" ] || fail "a wrong password returned ${code}, want 401"
grep -q 'invalid_credentials' "${WORK}/login.json" || fail "a wrong password did not answer invalid_credentials"
grep -q 'Invalid username or password.' "${WORK}/login.json" || fail "the failed sign-in message is not the generic one"
cp "${WORK}/login.json" "${WORK}/login-wrong.json"
code="$(login nobody "${PW}")"
[ "${code}" = "401" ] || fail "an unknown user returned ${code}, want 401"
cmp -s "${WORK}/login.json" "${WORK}/login-wrong.json" || fail "unknown user and wrong password answered with different bodies"

# Sign in, read /api/me, then the CSRF token guards sign-out.
code="$(login Admin "${PW}")"
[ "${code}" = "200" ] || fail "sign-in returned ${code}, want 200: $(cat "${WORK}/login.json")"
COOKIE_LINE="$(grep -i '^set-cookie:' "${WORK}/login.hdr" | tr -d '\r')"
printf '%s' "${COOKIE_LINE}" | grep -q '^[Ss]et-[Cc]ookie: __Host-tierplan_session=' || fail "sign-in set no __Host- cookie: ${COOKIE_LINE}"
if printf '%s' "${COOKIE_LINE}" | grep -qiE 'max-age|expires='; then fail "a sign-in without remember me set a persistent cookie: ${COOKIE_LINE}"; fi
SESSION2="$(printf '%s' "${COOKIE_LINE}" | sed 's/^[^=]*=//; s/;.*//')"
CSRF2="$(sed 's/.*"csrfToken":"\([^"]*\)".*/\1/' "${WORK}/login.json")"
[ -n "${CSRF2}" ] || fail "sign-in returned no CSRF token"
code="$(tcurl -sS -o /dev/null -w '%{http_code}' -H "Cookie: __Host-tierplan_session=${SESSION2}" "${BASE}/api/me")"
[ "${code}" = "200" ] || fail "/api/me after sign-in returned ${code}, want 200"
code="$(tcurl -sS -o "${WORK}/csrf.json" -w '%{http_code}' -X POST -H "Origin: ${ORIGIN}" -H "Cookie: __Host-tierplan_session=${SESSION2}" "${BASE}/api/auth/logout")"
[ "${code}" = "403" ] || fail "logout without the CSRF token returned ${code}, want 403"
grep -q 'csrf_failed' "${WORK}/csrf.json" || fail "logout without the CSRF token did not answer csrf_failed"
code="$(tcurl -sS -o /dev/null -D "${WORK}/logout.hdr" -w '%{http_code}' -X POST -H "Origin: ${ORIGIN}" -H "X-CSRF-Token: ${CSRF2}" \
  -H "Cookie: __Host-tierplan_session=${SESSION2}" "${BASE}/api/auth/logout")"
[ "${code}" = "204" ] || fail "logout with the CSRF token returned ${code}, want 204"
grep -i '^set-cookie:' "${WORK}/logout.hdr" | grep -qi 'max-age=0' || fail "logout did not clear the cookie"
code="$(tcurl -sS -o /dev/null -w '%{http_code}' -H "Cookie: __Host-tierplan_session=${SESSION2}" "${BASE}/api/me")"
[ "${code}" = "401" ] || fail "/api/me after logout returned ${code}, want 401"
code="$(tcurl -sS -o /dev/null -w '%{http_code}' -H "Cookie: __Host-tierplan_session=${SESSION_ID}" "${BASE}/api/me")"
[ "${code}" = "200" ] || fail "signing out one device ended the other (${code}), want 200"
if grep -qF "${SESSION2}" "${LOG}" || grep -qaF "${SESSION2}" "${DATA}"/tierplan.db*; then
  fail "the raw session ID of a sign-in reached the log or the database"
fi

# Lockout (D-29): five wrong passwords lock the account, the right password is
# then refused with the same answer, and `tierplan user unlock` lets the admin back in.
for _ in 1 2 3 4 5; do login admin 'wrong wrong wrong' >/dev/null; done
code="$(login admin "${PW}")"
[ "${code}" = "401" ] || fail "the right password on a locked account returned ${code}, want 401"
cmp -s "${WORK}/login.json" "${WORK}/login-wrong.json" || fail "a locked account answered differently from a wrong password"
grep -q 'account locked' "${LOG}" || fail "the lock was not written to the container log"
grep -q 'auth.locked' "${LOG}" || fail "auth.locked was not logged"
UNLOCK_OUT="$(TIERPLAN_DATA_DIR="${DATA}" "${BIN}" user unlock admin)" || fail "tierplan user unlock admin failed"
[ "${UNLOCK_OUT}" = "unlocked admin" ] || fail "tierplan user unlock printed '${UNLOCK_OUT}', want 'unlocked admin'"
code="$(login admin "${PW}")"
[ "${code}" = "200" ] || fail "sign-in after unlock returned ${code}, want 200"
if TIERPLAN_DATA_DIR="${DATA}" "${BIN}" user unlock nobody >"${WORK}/unlock.out" 2>"${WORK}/unlock.err"; then
  fail "tierplan user unlock nobody exited 0"
fi
grep -q '^no user named nobody$' "${WORK}/unlock.err" || fail "tierplan user unlock nobody did not say 'no user named nobody'"
TIERPLAN_DATA_DIR="${DATA}" "${BIN}" audit verify | grep -Eq '^audit chain OK: [0-9]+ entries$' \
  || fail "the audit chain is broken after sign-ins and a CLI unlock"

# The healthcheck subcommand, as the container HEALTHCHECK runs it.
TIERPLAN_DATA_DIR="${DATA}" TIERPLAN_LISTEN="127.0.0.1:${PORT}" "${BIN}" healthcheck \
  || fail "tierplan healthcheck exited non-zero while the server is up"

# A bad setting stops startup and names the env var (D-11).
if TIERPLAN_PUBLIC_URL=nope TIERPLAN_DATA_DIR="${WORK}/bad" "${BIN}" serve --listen "127.0.0.1:0" >"${WORK}/bad.log" 2>&1; then
  fail "serve accepted TIERPLAN_PUBLIC_URL=nope"
fi
grep -q 'TIERPLAN_PUBLIC_URL' "${WORK}/bad.log" || fail "startup error does not name TIERPLAN_PUBLIC_URL"

# After the server stops, the healthcheck must fail.
kill "${PID}"
wait "${PID}" 2>/dev/null || true
PID=""
if TIERPLAN_DATA_DIR="${DATA}" TIERPLAN_LISTEN="127.0.0.1:${PORT}" "${BIN}" healthcheck 2>/dev/null; then
  fail "tierplan healthcheck exited 0 after the server stopped"
fi

# Restart on the same data directory (PLAT-01): state is kept, /readyz is 200
# and no migration re-runs, so no pre-migration snapshot is taken (D-83).
LOG="${WORK}/restart.log"
(cd "${WORK}" && TIERPLAN_DATA_DIR="${DATA}" exec "${BIN}" serve --listen "127.0.0.1:${LISTEN_PORT}") >"${LOG}" 2>&1 &
PID=$!
await_listening "the restarted server"
up=""
for _ in $(seq 1 100); do
  if tcurl -fsS "${BASE}/readyz" -o "${WORK}/readyz2.txt" 2>/dev/null; then
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
tcurl -fsS "${BASE}/api/instance" -o "${WORK}/instance3.json" || fail "/api/instance did not answer after the restart"
grep -q '"needsSetup":false' "${WORK}/instance3.json" || fail "the restart reopened setup"
grep -q '"instanceName":"Smoke lab"' "${WORK}/instance3.json" || fail "the instance name did not survive the restart"
[ ! -e "${DATA}/setup-token" ] || fail "the restart wrote a setup-token although an admin exists"
if grep -q '"snapshot"' "${LOG}"; then
  fail "the restart on the same data directory re-ran a migration (a snapshot was logged)"
fi
[ ! -d "${DATA}/backups" ] || [ -z "$(ls -A "${DATA}/backups")" ] || fail "the restart left a snapshot in ${DATA}/backups"

echo "SMOKE OK"
