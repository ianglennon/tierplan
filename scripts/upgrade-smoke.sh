#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# The upgrade proof an operator would run by hand (D-34, PLAT-02): put real state
# into the PREVIOUS build's image, replace the container with the CURRENT image on
# the same /data volume and the same network alias, and check that everything
# survived. `make upgrade-smoke UPGRADE_BASE_REF=<commit>` builds both images and
# runs this; until v0.1.0 there is no released image, so the baseline is a commit
# (the pull request's base, or the previous commit on a push) built locally and
# nothing is pulled from a registry.
#
# What it does:
#   1. runs BASE_IMAGE on a new volume and network (alias "tierplan"), completes
#      setup through the API, adds an agent environment and connects a real agent
#      (AGENT_IMAGE) that dials TierPlan by that alias, and waits until the
#      environment is online;
#   2. when the base answers POST /api/users with 201 it also seeds a user (who
#      accepts the invite and chooses a password), a team with that user in it and
#      a Viewer grant on the environment; a base without the users API (404 or
#      405) is reported as SKIP, any other answer fails;
#   3. runs `tierplan audit verify` in the base container, then `docker rm -f`s the
#      server and keeps the volume and the agent;
#   4. runs HEAD_IMAGE with the same name, alias, volume and settings and checks:
#      /readyz is 200, the first admin signs in and /api/me reports instanceAdmin
#      true, the agent environment is online again by itself within
#      RECONNECT_WAIT seconds, the seeded user signs in with the grant, team and
#      membership intact, and `tierplan audit verify` prints "audit chain OK".
#
# One PASS or FAIL line per check. A failing check prints "UPGRADE SMOKE FAIL: ..."
# with the last 50 log lines of the server and the agent and exits 1; a clean run
# ends with the line "UPGRADE SMOKE PASS". Every container, network and volume
# created here is removed on exit, whatever happens.
#
# Environment: BASE_IMAGE (default tierplan:base), HEAD_IMAGE (tierplan:e2e),
# AGENT_IMAGE (tierplan-agent:dev), SMOKE_PORT (published on 127.0.0.1; by default Docker
# assigns it and it is read back after every server start) and RECONNECT_WAIT (120).
# Every container, network and volume carries the label
# tierplan.ci.owner=$TIERPLAN_CI_OWNER (local-<pid> when unset) so `make ci-clean`
# removes what a cancelled CI job left (D-23). Needs Docker, curl and jq.
#
# The throwaway containers talk plain HTTP on a private network, so both the
# server and the agent get TIERPLAN_AGENT_ALLOW_INSECURE=true, the development
# switch (docs/agent.md). The passwords and tokens belong to containers that are
# destroyed at the end; no repository secret is involved.
set -euo pipefail

cd "$(dirname "$0")/.."

BASE_IMAGE="${BASE_IMAGE:-tierplan:base}"
HEAD_IMAGE="${HEAD_IMAGE:-tierplan:e2e}"
AGENT_IMAGE="${AGENT_IMAGE:-tierplan-agent:dev}"
OWNER_LABEL="tierplan.ci.owner=${TIERPLAN_CI_OWNER:-local-$$}"
RECONNECT_WAIT="${RECONNECT_WAIT:-120}"

for tool in docker curl jq; do
  command -v "${tool}" >/dev/null || { echo "UPGRADE SMOKE FAIL: ${tool} is required" >&2; exit 1; }
done
for image in "${BASE_IMAGE}" "${HEAD_IMAGE}" "${AGENT_IMAGE}"; do
  docker image inspect "${image}" >/dev/null 2>&1 \
    || { echo "UPGRADE SMOKE FAIL: the image ${image} is not built; run make upgrade-smoke" >&2; exit 1; }
done

RUN_ID="$$"
NET="tierplan-upgrade-${RUN_ID}"
VOLUME="tierplan-upgrade-${RUN_ID}"
AGENT_VOLUME="tierplan-upgrade-agent-${RUN_ID}"
SERVER="tierplan-upgrade-server-${RUN_ID}"
AGENT="tierplan-upgrade-agent-${RUN_ID}"
ALIAS="tierplan"
PUBLIC="http://${ALIAS}:7480"
HOST_URL="" # set by start_server from the port Docker assigned
WORK="$(mktemp -d)"
JSON_H='Content-Type: application/json'
ADMIN_PW='correct horse battery'
BOB_PW='another long passphrase'

cleanup() {
  docker rm -f "${SERVER}" "${AGENT}" >/dev/null 2>&1 || true
  docker network rm "${NET}" >/dev/null 2>&1 || true
  docker volume rm -f "${VOLUME}" "${AGENT_VOLUME}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pass() { echo "PASS: $*"; }
skip() { echo "SKIP $*"; }

# fail CHECK [DETAIL] prints the failing check and the last 50 log lines of the
# server and the agent, then ends the run.
fail() {
  echo "UPGRADE SMOKE FAIL: $1${2:+ ($2)}" >&2
  local c
  for c in "${SERVER}" "${AGENT}"; do
    echo "--- last 50 lines of ${c} ---" >&2
    docker logs --tail 50 "${c}" >&2 2>&1 || true
  done
  exit 1
}

docker network create --label "${OWNER_LABEL}" "${NET}" >/dev/null
docker volume create --label "${OWNER_LABEL}" "${VOLUME}" >/dev/null
docker volume create --label "${OWNER_LABEL}" "${AGENT_VOLUME}" >/dev/null

# start_server IMAGE runs the server from IMAGE on the shared volume and alias.
# The 3 second health interval only speeds up the wait; the probe is the one
# baked into the image.
start_server() {
  docker run -d --name "${SERVER}" --label "${OWNER_LABEL}" \
    --network "${NET}" --network-alias "${ALIAS}" \
    -p "127.0.0.1:${SMOKE_PORT:-}:7480" \
    -v "${VOLUME}:/data" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e "TIERPLAN_PUBLIC_URL=${PUBLIC}" \
    -e TIERPLAN_AGENT_ALLOW_INSECURE=true \
    --health-interval 3s --health-start-period 0s --health-retries 5 \
    "$1" >/dev/null || return 1
  # Docker assigned the host port (unless SMOKE_PORT is set); read it back, because
  # every swap of the server gets a new one.
  HOST_URL="http://127.0.0.1:$(docker port "${SERVER}" 7480/tcp | head -n 1 | sed 's/.*://')"
}

wait_healthy() {
  local state="" _
  for _ in $(seq 1 90); do
    state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${SERVER}" 2>/dev/null || true)"
    [ "${state}" = "healthy" ] && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "${SERVER}" 2>/dev/null || echo false)" = "true" ] || { state="exited"; break; }
    sleep 1
  done
  return 1
}

# api METHOD PATH [BODY] sends the session's cookie, CSRF token and Origin and
# sets HTTP_CODE and API_BODY. It is not run in a subshell.
COOKIE="" CSRF=""
api() {
  HTTP_CODE="$(curl -sS -o "${WORK}/api.out" -w '%{http_code}' -X "$1" -H "Origin: ${PUBLIC}" \
    ${CSRF:+-H "X-CSRF-Token: ${CSRF}"} ${COOKIE:+-H "Cookie: ${COOKIE}"} ${3:+-H "${JSON_H}" -d "$3"} "${HOST_URL}$2")" || HTTP_CODE="curl-failed"
  API_BODY="$(cat "${WORK}/api.out" 2>/dev/null || true)"
}

# login USER PASSWORD signs in and keeps the session for api. It returns 1 when
# the server does not answer 200.
login() {
  local code
  code="$(curl -sS -o "${WORK}/login.json" -D "${WORK}/login.hdr" -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
    -d "{\"username\":\"$1\",\"password\":\"$2\"}" "${HOST_URL}/api/auth/login")" || return 1
  [ "${code}" = "200" ] || return 1
  COOKIE="$(grep -i '^set-cookie:' "${WORK}/login.hdr" | head -n 1 | tr -d '\r' | sed 's/^[^:]*: *//; s/;.*//')"
  CSRF="$(jq -r .csrfToken "${WORK}/login.json")"
  [ -n "${COOKIE}" ] && [ "${CSRF}" != null ]
}

# wait_online ENV_ID waits up to RECONNECT_WAIT seconds for the environment's
# state to read online.
wait_online() {
  local _ state=""
  for _ in $(seq 1 "${RECONNECT_WAIT}"); do
    api GET /api/environments
    if [ "${HTTP_CODE}" = "200" ]; then
      state="$(printf '%s' "${API_BODY}" | jq -r --arg id "$1" '.items[] | select(.id == $id) | .status.state' 2>/dev/null || true)"
      [ "${state}" = "online" ] && return 0
    fi
    sleep 1
  done
  LAST_STATE="${state:-unknown}"
  return 1
}

audit_ok() {
  docker exec "${SERVER}" /usr/local/bin/tierplan audit verify 2>&1 | tee "${WORK}/audit.out" \
    | grep -Eq '^audit chain OK: [0-9]+ entries$'
}

# ---- 1. the base image: setup, an agent environment with a real agent ----

start_server "${BASE_IMAGE}" || fail "base starts" "docker run failed"
wait_healthy || fail "base healthy" "the container did not become healthy within 90 seconds"
pass "base healthy (${BASE_IMAGE})"

TOKEN="$(docker exec "${SERVER}" /usr/local/bin/tierplan setup-token)" || fail "setup token" "docker exec tierplan setup-token failed"
code="$(curl -sS -o "${WORK}/complete.json" -D "${WORK}/complete.hdr" -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
  -d "{\"token\":\"${TOKEN}\",\"username\":\"admin\",\"password\":\"${ADMIN_PW}\",\"instanceName\":\"Upgrade smoke\",\"publicUrl\":\"${PUBLIC}\"}" \
  "${HOST_URL}/api/setup/complete")" || code="curl-failed"
[ "${code}" = "200" ] || fail "setup done" "setup completion returned ${code}: $(cat "${WORK}/complete.json" 2>/dev/null)"
COOKIE="$(grep -i '^set-cookie:' "${WORK}/complete.hdr" | head -n 1 | tr -d '\r' | sed 's/^[^:]*: *//; s/;.*//')"
CSRF="$(jq -r .csrfToken "${WORK}/complete.json")"
[ -n "${COOKIE}" ] && [ "${CSRF}" != null ] || fail "setup done" "setup completion returned no session or CSRF token"
pass "setup done"

api POST /api/environments '{"kind":"agent","name":"smoke-agent"}'
[ "${HTTP_CODE}" = "201" ] || fail "agent environment added" "POST /api/environments returned ${HTTP_CODE}: ${API_BODY}"
ENV_ID="$(printf '%s' "${API_BODY}" | jq -r .id)"
JOIN_TOKEN="$(printf '%s' "${API_BODY}" | jq -r .install.joinToken)"
AGENT_URL="$(printf '%s' "${API_BODY}" | jq -r .install.agentUrl)"
{ [ -n "${ENV_ID}" ] && [ "${ENV_ID}" != null ] && [ -n "${JOIN_TOKEN}" ] && [ "${JOIN_TOKEN}" != null ] && [ -n "${AGENT_URL}" ] && [ "${AGENT_URL}" != null ]; } \
  || fail "agent environment added" "the answer carries no environment, join token or agent URL"

# The agent runs the way the standalone install command runs it, on the same
# network so that it reaches TierPlan by its alias.
docker run -d --name "${AGENT}" --network "${NET}" --label "${OWNER_LABEL}" \
  --read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID --security-opt no-new-privileges \
  -v /var/run/docker.sock:/var/run/docker.sock -v "${AGENT_VOLUME}:/var/lib/tierplan-agent" \
  --label tierplan.agent=true \
  -e "TIERPLAN_AGENT_URL=${AGENT_URL}" -e "TIERPLAN_AGENT_JOIN_TOKEN=${JOIN_TOKEN}" \
  -e TIERPLAN_AGENT_ALLOW_INSECURE=true \
  "${AGENT_IMAGE}" >/dev/null || fail "agent starts" "docker run failed"
wait_online "${ENV_ID}" || fail "agent environment online on base" "state ${LAST_STATE} after ${RECONNECT_WAIT} seconds"
pass "agent environment online on base"

# ---- 2. users, a team and a grant, when the base has them ----

SEEDED=false
BOB_ID="" TEAM_ID=""
api POST /api/users '{"username":"bob","displayName":"Bob"}'
case "${HTTP_CODE}" in
  201)
    BOB_ID="$(printf '%s' "${API_BODY}" | jq -r .user.id)"
    INVITE_URL="$(printf '%s' "${API_BODY}" | jq -r .link.url)"
    INVITE_TOKEN="${INVITE_URL##*#token=}"
    [ -n "${INVITE_TOKEN}" ] && [ "${INVITE_TOKEN}" != "${INVITE_URL}" ] || fail "users seeded" "the invite link carries no token: ${INVITE_URL}"
    code="$(curl -sS -o "${WORK}/accept.out" -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
      -d "{\"token\":\"${INVITE_TOKEN}\",\"password\":\"${BOB_PW}\"}" "${HOST_URL}/api/invite/accept")" || code="curl-failed"
    [ "${code}" = "204" ] || fail "users seeded" "accepting the invite returned ${code}: $(cat "${WORK}/accept.out")"
    api POST /api/teams '{"name":"ops"}'
    [ "${HTTP_CODE}" = "201" ] || fail "users seeded" "creating the team returned ${HTTP_CODE}: ${API_BODY}"
    TEAM_ID="$(printf '%s' "${API_BODY}" | jq -r .id)"
    api POST "/api/teams/${TEAM_ID}/members" "{\"userIds\":[\"${BOB_ID}\"]}"
    case "${HTTP_CODE}" in 200 | 201) ;; *) fail "users seeded" "adding bob to the team returned ${HTTP_CODE}: ${API_BODY}" ;; esac
    api PUT "/api/users/${BOB_ID}/grants" "{\"scope\":\"env\",\"envId\":\"${ENV_ID}\",\"role\":\"viewer\"}"
    case "${HTTP_CODE}" in 200 | 201) ;; *) fail "users seeded" "granting Viewer returned ${HTTP_CODE}: ${API_BODY}" ;; esac
    SEEDED=true
    pass "users seeded (bob, team ops, Viewer on the environment)"
    ;;
  404 | 405)
    skip "users: base has no users API"
    ;;
  *)
    fail "users seeded" "POST /api/users returned ${HTTP_CODE}: ${API_BODY}"
    ;;
esac

audit_ok || fail "audit verify on base" "$(cat "${WORK}/audit.out")"
pass "audit verify on base"

# ---- 3. the swap: the server goes, the volume and the agent stay ----

docker rm -f "${SERVER}" >/dev/null || fail "base removed" "docker rm -f failed"
COOKIE="" CSRF=""
start_server "${HEAD_IMAGE}" || fail "head healthy on the same volume" "docker run failed"
wait_healthy || fail "head healthy on the same volume" "the container did not become healthy within 90 seconds"
pass "head healthy on the same volume (${HEAD_IMAGE})"

code="$(curl -sS -o /dev/null -w '%{http_code}' "${HOST_URL}/readyz")" || code="curl-failed"
[ "${code}" = "200" ] || fail "readyz" "/readyz returned ${code}, want 200"
pass "readyz 200"

login admin "${ADMIN_PW}" || fail "admin sign-in" "the first admin could not sign in after the swap"
pass "admin sign-in"

api GET /api/me
[ "${HTTP_CODE}" = "200" ] || fail "instanceAdmin true" "/api/me returned ${HTTP_CODE}: ${API_BODY}"
printf '%s' "${API_BODY}" | jq -e '.user.instanceAdmin == true' >/dev/null \
  || fail "instanceAdmin true" "/api/me says: $(printf '%s' "${API_BODY}" | jq -c .user)"
pass "instanceAdmin true"

# The head server reads the environment from the migrated database; its agent
# redials by itself (the reconnect grace, D-41).
wait_online "${ENV_ID}" || fail "environment online again" "state ${LAST_STATE} after ${RECONNECT_WAIT} seconds"
pass "environment online again"

if [ "${SEEDED}" = true ]; then
  api GET "/api/users/${BOB_ID}/access"
  [ "${HTTP_CODE}" = "200" ] || fail "seeded grant intact" "GET /api/users/${BOB_ID}/access returned ${HTTP_CODE}: ${API_BODY}"
  printf '%s' "${API_BODY}" | jq -e --arg env "${ENV_ID}" '[.grants[] | select(.scope == "env" and .envId == $env and .role == "viewer")] | length == 1' >/dev/null \
    || fail "seeded grant intact" "bob's grants: $(printf '%s' "${API_BODY}" | jq -c .grants)"
  api GET "/api/teams/${TEAM_ID}"
  [ "${HTTP_CODE}" = "200" ] || fail "seeded team intact" "GET /api/teams/${TEAM_ID} returned ${HTTP_CODE}: ${API_BODY}"
  printf '%s' "${API_BODY}" | jq -e --arg id "${BOB_ID}" '[.members[] | select(.userId == $id)] | length == 1' >/dev/null \
    || fail "seeded team intact" "the team's members: $(printf '%s' "${API_BODY}" | jq -c '[.members[].username]')"
  COOKIE="" CSRF=""
  login bob "${BOB_PW}" || fail "seeded user sign-in" "bob could not sign in after the swap"
  api GET /api/environments
  [ "${HTTP_CODE}" = "200" ] || fail "seeded user sign-in" "bob's environment list returned ${HTTP_CODE}: ${API_BODY}"
  printf '%s' "${API_BODY}" | jq -e --arg id "${ENV_ID}" '[.items[] | select(.id == $id)] | length == 1' >/dev/null \
    || fail "seeded user sign-in" "bob cannot see the environment he was granted"
  api GET /api/me
  printf '%s' "${API_BODY}" | jq -e '.user.instanceAdmin == false' >/dev/null \
    || fail "seeded user sign-in" "bob must not be an instance admin: $(printf '%s' "${API_BODY}" | jq -c .user)"
  pass "seeded user sign-in and grant intact"
else
  skip "seeded user sign-in: nothing was seeded"
fi

audit_ok || fail "audit verify on head" "$(cat "${WORK}/audit.out")"
pass "audit verify on head (audit chain OK)"

echo "UPGRADE SMOKE PASS"
