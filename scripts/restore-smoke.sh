#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# The PID 1 spike for a restore from the web UI (D-30, PLAT-03, RESEARCH A3 and
# A5). After Settings > Backup > Restore swaps the database, Bayplan restarts by
# re-executing itself (syscall.Exec), so the container's PID 1 survives and the
# container needs no restart policy. Whether exec works for PID 1 after the
# privilege drop (root to UID 1000 inside a distroless image) can only be
# answered in the shipped image, so this script asks it:
#
#   1. runs IMAGE on a new volume with NO restart policy and completes setup as
#      the admin;
#   2. creates user member1 (who accepts the invite and chooses a password), exports a
#      backup, then creates user member2 the same way, so member2 is not in the backup;
#   3. records `docker inspect` State.Pid, State.StartedAt and RestartCount;
#   4. uploads the backup, checks it and applies it with the instance name, then
#      waits until Bayplan has restarted into the restored state (the log says
#      "restore completed" and /readyz answers 200, up to RESTORE_WAIT seconds);
#   5. asserts the container is still running with the same State.Pid,
#      State.StartedAt and RestartCount (the same process tree: exec kept PID 1),
#      the session from before the restore is refused, the admin and member1 sign in,
#      member2 is refused, the pre-restore snapshot is in /data/backups, the
#      restore-pending marker is gone, and `bayplan audit verify` prints
#      "audit chain OK".
#
# The decision rule (plan 03-25): if this passes, exec stays the default of
# BAYPLAN_RESTORE_RESTART. If the exec step fails in the image (the container
# exits or restarts), the default becomes "exit" and this script is run with
# RESTART_MODE=exit, which starts the container with --restart unless-stopped and
# BAYPLAN_RESTORE_RESTART=exit and expects exactly one restart (RestartCount up by
# one, a new StartedAt) instead. Both modes stay tested.
#
# One PASS or FAIL line per check. A failing check prints "RESTORE SMOKE FAIL: ..."
# with the last 50 log lines and exits 1; a clean run ends with the line
# "RESTORE SMOKE PASS". The container and volume are removed on exit.
#
# Environment: IMAGE (default bayplan:e2e, built by `make e2e-image`),
# SMOKE_PORT (default: a free loopback port the kernel assigns, published on
# 127.0.0.1), RESTORE_WAIT (90) and RESTART_MODE (exec or exit; default exec). The
# container and volume carry the label bayplan.ci.owner=$BAYPLAN_CI_OWNER
# (local-<pid> when unset) so `make ci-clean` removes them after a cancelled CI job
# (D-23). Needs Docker, curl, jq, tar and python3. The passwords
# belong to a container that is destroyed at the end; no repository secret is
# involved.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-bayplan:e2e}"
# free_port prints a TCP port the kernel has just handed out on loopback, so no fixed
# port is ever in the way (D-23). SMOKE_PORT still overrides it.
free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

PORT="${SMOKE_PORT:-$(free_port)}"
OWNER_LABEL="bayplan.ci.owner=${BAYPLAN_CI_OWNER:-local-$$}"
RESTORE_WAIT="${RESTORE_WAIT:-90}"
RESTART_MODE="${RESTART_MODE:-exec}"
case "${RESTART_MODE}" in
  exec | exit) ;;
  *) echo "RESTORE SMOKE FAIL: RESTART_MODE must be exec or exit, not ${RESTART_MODE}" >&2; exit 1 ;;
esac

for tool in docker curl jq tar python3; do
  command -v "${tool}" >/dev/null || { echo "RESTORE SMOKE FAIL: ${tool} is required" >&2; exit 1; }
done
docker image inspect "${IMAGE}" >/dev/null 2>&1 \
  || { echo "RESTORE SMOKE FAIL: the image ${IMAGE} is not built; run make restore-smoke" >&2; exit 1; }

NAME="bayplan-restore-smoke-$$"
VOLUME="${NAME}"
PUBLIC="http://127.0.0.1:${PORT}"
WORK="$(mktemp -d)"
JSON_H='Content-Type: application/json'
ADMIN_PW='correct horse battery'
M1_PW='another long passphrase'
M2_PW='yet another passphrase'
BACKUP_PW='restore smoke passphrase'
INSTANCE='Restore smoke'

cleanup() {
  docker rm -f "${NAME}" >/dev/null 2>&1 || true
  docker volume rm -f "${VOLUME}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass() { echo "PASS: $*"; }

fail() {
  echo "RESTORE SMOKE FAIL: $1${2:+ ($2)}" >&2
  echo "--- last 50 lines of ${NAME} ---" >&2
  docker logs --tail 50 "${NAME}" >&2 2>&1 || true
  exit 1
}

# inspect FIELD prints one field of `docker inspect` for the container.
inspect() { docker inspect -f "$1" "${NAME}" 2>/dev/null; }

# api METHOD PATH [BODY] sends the session's cookie, CSRF token and Origin and
# sets HTTP_CODE and API_BODY. It is not run in a subshell.
COOKIE="" CSRF=""
api() {
  HTTP_CODE="$(curl -sS -o "${WORK}/api.out" -w '%{http_code}' -X "$1" -H "Origin: ${PUBLIC}" \
    ${CSRF:+-H "X-CSRF-Token: ${CSRF}"} ${COOKIE:+-H "Cookie: ${COOKIE}"} ${3:+-H "${JSON_H}" -d "$3"} "${PUBLIC}$2")" || HTTP_CODE="curl-failed"
  API_BODY="$(cat "${WORK}/api.out" 2>/dev/null || true)"
}

# login USER PASSWORD signs in and keeps the session for api. It returns 1 when
# the server does not answer 200.
login() {
  local code
  code="$(curl -sS -o "${WORK}/login.json" -D "${WORK}/login.hdr" -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}" "${PUBLIC}/api/auth/login")" || return 1
  [ "${code}" = "200" ] || return 1
  COOKIE="$(grep -i '^set-cookie:' "${WORK}/login.hdr" | head -n 1 | tr -d '\r' | sed 's/^[^:]*: *//; s/;.*//')"
  CSRF="$(jq -r .csrfToken "${WORK}/login.json")"
  [ -n "${COOKIE}" ] && [ "${CSRF}" != null ]
}

# new_member USERNAME PASSWORD creates the user through the API and accepts the
# invite, as the person would.
new_member() {
  local invite token code
  api POST /api/users "{\"username\":\"$1\",\"displayName\":\"$1\"}"
  [ "${HTTP_CODE}" = "201" ] || fail "user $1 created" "POST /api/users returned ${HTTP_CODE}: ${API_BODY}"
  invite="$(printf '%s' "${API_BODY}" | jq -r .link.url)"
  token="${invite##*#token=}"
  [ -n "${token}" ] && [ "${token}" != "${invite}" ] || fail "user $1 created" "the invite link carries no token: ${invite}"
  code="$(curl -sS -o "${WORK}/accept.out" -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
    -d "{\"token\":\"${token}\",\"password\":\"$2\"}" "${PUBLIC}/api/invite/accept")" || code="curl-failed"
  [ "${code}" = "204" ] || fail "user $1 created" "accepting the invite returned ${code}: $(cat "${WORK}/accept.out")"
}

wait_healthy() {
  local state="" _
  for _ in $(seq 1 90); do
    state="$(inspect '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' || true)"
    [ "${state}" = "healthy" ] && return 0
    [ "$(inspect '{{.State.Running}}' || echo false)" = "true" ] || break
    sleep 1
  done
  return 1
}

docker volume create --label "${OWNER_LABEL}" "${VOLUME}" >/dev/null

RUN_ARGS=()
if [ "${RESTART_MODE}" = "exit" ]; then
  RUN_ARGS+=(--restart unless-stopped -e BAYPLAN_RESTORE_RESTART=exit)
fi
# The 3 second health interval only speeds up the wait; the probe is the one
# baked into the image. There is deliberately no restart policy in exec mode.
docker run -d --name "${NAME}" --label "${OWNER_LABEL}" \
  -p "127.0.0.1:${PORT}:7480" \
  -v "${VOLUME}:/data" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e "BAYPLAN_PUBLIC_URL=${PUBLIC}" \
  --health-interval 3s --health-start-period 0s --health-retries 5 \
  ${RUN_ARGS[@]+"${RUN_ARGS[@]}"} \
  "${IMAGE}" >/dev/null || fail "image starts" "docker run failed"
wait_healthy || fail "image healthy" "the container did not become healthy within 90 seconds"
pass "image healthy (${IMAGE}, restart mode ${RESTART_MODE})"

# ---- 1. setup, two members and a backup in between ----

TOKEN="$(docker exec "${NAME}" /usr/local/bin/bayplan setup-token)" || fail "setup token" "docker exec bayplan setup-token failed"
code="$(curl -sS -o "${WORK}/complete.json" -D "${WORK}/complete.hdr" -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
  -d "{\"token\":\"${TOKEN}\",\"username\":\"admin\",\"password\":\"${ADMIN_PW}\",\"instanceName\":\"${INSTANCE}\",\"publicUrl\":\"${PUBLIC}\"}" \
  "${PUBLIC}/api/setup/complete")" || code="curl-failed"
[ "${code}" = "200" ] || fail "setup done" "setup completion returned ${code}: $(cat "${WORK}/complete.json" 2>/dev/null)"
COOKIE="$(grep -i '^set-cookie:' "${WORK}/complete.hdr" | head -n 1 | tr -d '\r' | sed 's/^[^:]*: *//; s/;.*//')"
CSRF="$(jq -r .csrfToken "${WORK}/complete.json")"
[ -n "${COOKIE}" ] && [ "${CSRF}" != null ] || fail "setup done" "setup completion returned no session or CSRF token"
pass "setup done"

new_member member1 "${M1_PW}"
pass "user member1 created"

code="$(curl -sS -o "${WORK}/backup.bayplan" -w '%{http_code}' -X POST -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
  -H "X-CSRF-Token: ${CSRF}" -H "Cookie: ${COOKIE}" -d "{\"passphrase\":\"${BACKUP_PW}\"}" "${PUBLIC}/api/backup/export")" || code="curl-failed"
[ "${code}" = "200" ] || fail "backup exported" "POST /api/backup/export returned ${code}"
[ -s "${WORK}/backup.bayplan" ] || fail "backup exported" "the downloaded file is empty"
pass "backup exported ($(wc -c <"${WORK}/backup.bayplan") bytes)"

new_member member2 "${M2_PW}"
pass "user member2 created after the backup"

# ---- 2. the restore ----

BEFORE_PID="$(inspect '{{.State.Pid}}')"
BEFORE_STARTED="$(inspect '{{.State.StartedAt}}')"
BEFORE_RESTARTS="$(inspect '{{.RestartCount}}')"
[ "${BEFORE_PID}" -gt 0 ] || fail "container state recorded" "State.Pid is ${BEFORE_PID}"
pass "container state recorded (pid ${BEFORE_PID}, started ${BEFORE_STARTED}, restarts ${BEFORE_RESTARTS})"
OLD_COOKIE="${COOKIE}"

code="$(curl -sS -o "${WORK}/upload.json" -w '%{http_code}' -X POST -H "Content-Type: application/octet-stream" -H "Origin: ${PUBLIC}" \
  -H "X-CSRF-Token: ${CSRF}" -H "Cookie: ${COOKIE}" --data-binary "@${WORK}/backup.bayplan" "${PUBLIC}/api/backup/restore/upload")" || code="curl-failed"
[ "${code}" = "201" ] || fail "backup uploaded" "upload returned ${code}: $(cat "${WORK}/upload.json" 2>/dev/null)"
UPLOAD_ID="$(jq -r .uploadId "${WORK}/upload.json")"
[ -n "${UPLOAD_ID}" ] && [ "${UPLOAD_ID}" != null ] || fail "backup uploaded" "the answer carries no upload ID"
pass "backup uploaded"

api POST /api/backup/restore/check "{\"uploadId\":\"${UPLOAD_ID}\",\"passphrase\":\"${BACKUP_PW}\"}"
[ "${HTTP_CODE}" = "200" ] || fail "backup checked" "check returned ${HTTP_CODE}: ${API_BODY}"
printf '%s' "${API_BODY}" | jq -e --arg n "${INSTANCE}" '.instanceName == $n and .counts.users == 2' >/dev/null \
  || fail "backup checked" "the summary does not describe the backup: ${API_BODY}"
pass "backup checked (2 users)"

api POST /api/backup/restore/apply "{\"uploadId\":\"${UPLOAD_ID}\",\"passphrase\":\"${BACKUP_PW}\",\"confirm\":\"${INSTANCE}\"}"
[ "${HTTP_CODE}" = "202" ] || fail "restore accepted" "apply returned ${HTTP_CODE}: ${API_BODY}"
pass "restore accepted (202)"

# Wait for the restored Bayplan: the log says so, and it answers. The log is the
# container's, so it holds the old process and the new one.
RESTORED=false
for _ in $(seq 1 "${RESTORE_WAIT}"); do
  if docker logs "${NAME}" 2>&1 | grep -q '"restore completed"' \
    && [ "$(curl -sS -o /dev/null -w '%{http_code}' "${PUBLIC}/readyz" 2>/dev/null || true)" = "200" ]; then
    RESTORED=true
    break
  fi
  sleep 1
done
[ "${RESTORED}" = true ] || fail "restarted into the restored state" "no \"restore completed\" and /readyz 200 within ${RESTORE_WAIT} seconds"
pass "restarted into the restored state"

# ---- 3. what the restart did to the container ----

[ "$(inspect '{{.State.Running}}')" = "true" ] || fail "container still running" "State.Running is $(inspect '{{.State.Status}}')"
AFTER_RESTARTS="$(inspect '{{.RestartCount}}')"
AFTER_STARTED="$(inspect '{{.State.StartedAt}}')"
AFTER_PID="$(inspect '{{.State.Pid}}')"
if [ "${RESTART_MODE}" = "exec" ]; then
  [ "${AFTER_RESTARTS}" = "${BEFORE_RESTARTS}" ] || fail "RestartCount unchanged" "was ${BEFORE_RESTARTS}, now ${AFTER_RESTARTS}: the container restarted"
  [ "${AFTER_STARTED}" = "${BEFORE_STARTED}" ] || fail "StartedAt unchanged" "was ${BEFORE_STARTED}, now ${AFTER_STARTED}: the container restarted"
  [ "${AFTER_PID}" = "${BEFORE_PID}" ] || fail "State.Pid unchanged" "was ${BEFORE_PID}, now ${AFTER_PID}: PID 1 did not survive"
  pass "same container process tree (PID 1 survived the exec)"
else
  [ "${AFTER_RESTARTS}" = "$((BEFORE_RESTARTS + 1))" ] || fail "RestartCount up by one" "was ${BEFORE_RESTARTS}, now ${AFTER_RESTARTS}"
  [ "${AFTER_STARTED}" != "${BEFORE_STARTED}" ] || fail "StartedAt changed" "the container was not restarted"
  pass "restarted once by the restart policy (exit mode)"
fi

# ---- 4. the restored state ----

# The session from before the restore is gone with everyone else's.
COOKIE="${OLD_COOKIE}"
api GET /api/me
[ "${HTTP_CODE}" = "401" ] || fail "old session refused" "/api/me with the pre-restore cookie returned ${HTTP_CODE}"
pass "old session refused"

COOKIE="" CSRF=""
login admin "${ADMIN_PW}" || fail "admin sign-in" "the admin could not sign in after the restore"
pass "admin sign-in"
COOKIE="" CSRF=""
login member1 "${M1_PW}" || fail "member1 sign-in" "member1 was in the backup and could not sign in after the restore"
pass "member1 sign-in (in the backup)"
COOKIE="" CSRF=""
if login member2 "${M2_PW}"; then
  fail "member2 refused" "member2 was created after the backup and still signs in"
fi
pass "member2 refused (created after the backup)"

docker cp "${NAME}:/data/backups" - 2>/dev/null | tar -t 2>/dev/null | grep -Eq 'prerestore-[0-9]{8}T[0-9]{6}Z\.db$' \
  || fail "pre-restore snapshot kept" "no backups/prerestore-*.db in the data volume"
pass "pre-restore snapshot kept"

if docker cp "${NAME}:/data/restore-pending" - >/dev/null 2>&1; then
  fail "restore marker removed" "/data/restore-pending is still there"
fi
pass "restore marker removed"

docker exec "${NAME}" /usr/local/bin/bayplan audit verify 2>&1 | tee "${WORK}/audit.out" \
  | grep -Eq '^audit chain OK: [0-9]+ entries$' || fail "audit verify" "$(cat "${WORK}/audit.out")"
pass "audit verify (audit chain OK)"

echo "RESTORE SMOKE PASS"
