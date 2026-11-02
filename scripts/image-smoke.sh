#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Runs the release image (`make image` builds it) the way a self-hoster does:
# one volume, the Docker socket, port 7480. It checks what a unit test cannot:
# the image's own HEALTHCHECK turns healthy, the process is UID 1000, a
# `docker exec` CLI call leaves no root-owned files, state survives
# `docker rm -f` on the same volume, and TierPlan finds its own container.
#
# Environment: IMAGE (default: the local ghcr.io/ianglennon/tierplan snapshot
# image for this engine's architecture), SMOKE_PORT (default: unset, so Docker
# assigns the loopback host port, which the script reads back after every start;
# TierPlan's public URL is a fixed one that does not name it) and EXTRA_RUN_ARGS
# (extra `docker run` flags, for example --hostname). Needs Docker, curl and jq.
# Removes every container and volume it creates, and labels them
# tierplan.ci.owner=$TIERPLAN_CI_OWNER (local-<pid> when unset) so `make ci-clean`
# can remove what a cancelled CI job left (D-23).
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-}"
if [ -z "${IMAGE}" ]; then
  # `make image` tags one image per platform (…-amd64, …-arm64); take the one
  # this engine can run.
  ARCH="$(docker version --format '{{.Server.Arch}}')"
  IMAGE="$(docker image ls ghcr.io/ianglennon/tierplan --format '{{.Repository}}:{{.Tag}}' \
    | grep -E -- "-snapshot-[0-9a-f]+-${ARCH}$" | head -n 1 || true)"
fi
[ -n "${IMAGE}" ] || { echo "IMAGE SMOKE FAIL: no local snapshot image for this engine; run make image first" >&2; exit 1; }

OWNER_LABEL="tierplan.ci.owner=${TIERPLAN_CI_OWNER:-local-$$}"
# The public URL the server is told is fixed; the host port Docker assigns is read
# back by start (never probed for beforehand, because two runners share each VM and
# a port found free can be taken before docker run), and requests go to BASE (the
# host URL) with the public URL as their Origin.
PUBLIC="http://127.0.0.1:7480"
BASE="" # set by start from the port Docker assigned
NAME="tierplan-image-smoke-$$"
VOLUME="${NAME}"
WORK="$(mktemp -d)"
JSON_H='Content-Type: application/json'
PW='correct horse battery'

cleanup() {
  docker rm -f "${NAME}" >/dev/null 2>&1 || true
  docker volume rm -f "${VOLUME}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

fail() {
  echo "IMAGE SMOKE FAIL: $1" >&2
  docker logs "${NAME}" 2>&1 | tail -n 40 >&2 || true
  exit 1
}

start() {
  # The 3 second health interval only speeds up the wait; the probe command is
  # the one baked into the image.
  docker run -d --name "${NAME}" --label "${OWNER_LABEL}" \
    -p "127.0.0.1:${SMOKE_PORT:-}:7480" \
    -v "${VOLUME}:/data" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e "TIERPLAN_PUBLIC_URL=${PUBLIC}" \
    --health-interval 3s --health-start-period 0s --health-retries 5 \
    ${EXTRA_RUN_ARGS:-} \
    "${IMAGE}" >/dev/null || fail "docker run failed"
  # Docker assigned the host port (unless SMOKE_PORT is set); every start gets a new one.
  BASE="http://127.0.0.1:$(docker port "${NAME}" 7480/tcp | head -n 1 | sed 's/.*://')"
}

wait_healthy() {
  local state=""
  for _ in $(seq 1 60); do
    state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${NAME}")"
    [ "${state}" = "healthy" ] && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "${NAME}")" = "true" ] || fail "the container exited while starting"
    sleep 1
  done
  fail "the container did not become healthy within 60 seconds (last state: ${state})"
}

docker volume create --label "${OWNER_LABEL}" "${VOLUME}" >/dev/null
start
wait_healthy

# 1. UID 1000 with no root process left: the privilege drop ran (D-17).
tops="$(docker top "${NAME}" -eo pid,uid,comm)"
printf '%s\n' "${tops}" | awk 'NR > 1 && $3 == "tierplan" { found = 1; if ($2 != "1000") bad = 1 } END { exit !(found && !bad) }' \
  || fail "the tierplan process is not UID 1000: ${tops}"

# 2. The published port answers (the Docker socket group is proved in step 4,
# where the dropped user adds this host).
code="$(curl -sS -o /dev/null -w '%{http_code}' "${BASE}/healthz")"
[ "${code}" = "200" ] || fail "/healthz returned ${code}, want 200"

# 3. `docker exec` runs as root; the CLI must drop too and leave nothing root-owned (A8).
TOKEN="$(docker exec "${NAME}" /usr/local/bin/tierplan setup-token)" || fail "docker exec tierplan setup-token failed"
printf '%s' "${TOKEN}" | grep -Eq '^[0-9a-f]{8}(-[0-9a-f]{8}){3}$' || fail "setup-token printed '${TOKEN}', not a token"
docker exec "${NAME}" /usr/local/bin/tierplan audit verify | grep -Eq '^audit chain OK: [0-9]+ entries$' \
  || fail "docker exec tierplan audit verify did not report an intact chain"
docker exec "${NAME}" /usr/local/bin/tierplan version >/dev/null || fail "docker exec tierplan version failed"
owners="$(docker cp "${NAME}:/data" - | tar -tvf - | awk '{ print $2 }' | sort -u)"
[ "${owners}" = "1000/1000" ] || fail "files under /data are not all owned by 1000/1000: ${owners}"

# 4. Setup, sign in and add this host.
code="$(curl -sS -o "${WORK}/complete.json" -D "${WORK}/complete.hdr" -w '%{http_code}' -H "${JSON_H}" \
  -d "{\"token\":\"${TOKEN}\",\"username\":\"admin\",\"password\":\"${PW}\",\"instanceName\":\"Image smoke\",\"publicUrl\":\"${PUBLIC}\"}" \
  "${BASE}/api/setup/complete")"
[ "${code}" = "200" ] || fail "setup completion returned ${code}: $(cat "${WORK}/complete.json")"
SESSION="$(grep -i '^set-cookie:' "${WORK}/complete.hdr" | tr -d '\r' | sed 's/^[^=]*=//; s/;.*//')"
CSRF="$(jq -r .csrfToken "${WORK}/complete.json")"
[ -n "${SESSION}" ] && [ "${CSRF}" != null ] || fail "setup completion returned no session or CSRF token"
# api METHOD PATH [BODY] sets HTTP_CODE and API_BODY. It is not run in a
# subshell, so the variables are visible to the caller.
api() {
  HTTP_CODE="$(curl -sS -o "${WORK}/api.out" -w '%{http_code}' -X "$1" -H "Origin: ${PUBLIC}" -H "X-CSRF-Token: ${CSRF}" \
    -H "Cookie: tierplan_session=${SESSION}" ${3:+-H "${JSON_H}" -d "$3"} "${BASE}$2")"
  API_BODY="$(cat "${WORK}/api.out")"
}
api POST /api/environments '{"kind":"local"}'
env_json="${API_BODY}"
[ "${HTTP_CODE}" = "201" ] || fail "adding the local host returned ${HTTP_CODE}: ${env_json}"
ENV_ID="$(printf '%s' "${env_json}" | jq -r .id)"

# 5. TierPlan finds its own container (D-27, RESEARCH A2, A9): compare with docker inspect.
FULL_ID="$(docker inspect -f '{{.Id}}' "${NAME}")"
api GET "/api/env/${ENV_ID}/summary"
summary="${API_BODY}"
[ "${HTTP_CODE}" = "200" ] || fail "the summary returned ${HTTP_CODE}: ${summary}"
[ "$(printf '%s' "${summary}" | jq -r .selfContainerId)" = "${FULL_ID}" ] \
  || fail "selfContainerId is '$(printf '%s' "${summary}" | jq -r .selfContainerId)', docker inspect says ${FULL_ID}"
api GET "/api/env/${ENV_ID}/containers"
containers="${API_BODY}"
[ "${HTTP_CODE}" = "200" ] || fail "the container list returned ${HTTP_CODE}: ${containers}"
selfs="$(printf '%s' "${containers}" | jq -r '[.items[] | select(.isSelf) | .id] | join(",")')"
[ "${selfs}" = "${FULL_ID}" ] || fail "isSelf marks '${selfs}', want only ${FULL_ID}"

# 6. State survives docker rm -f on the same volume.
docker rm -f "${NAME}" >/dev/null
start
wait_healthy
instance="$(curl -fsS "${BASE}/api/instance")" || fail "/api/instance did not answer after the re-run"
printf '%s' "${instance}" | jq -e '.needsSetup == false and .instanceName == "Image smoke"' >/dev/null \
  || fail "state did not survive docker rm -f: ${instance}"
code="$(curl -sS -o /dev/null -w '%{http_code}' -H "${JSON_H}" -H "Origin: ${PUBLIC}" \
  -d "{\"username\":\"admin\",\"password\":\"${PW}\"}" "${BASE}/api/auth/login")"
[ "${code}" = "200" ] || fail "signing in after the re-run returned ${code}, want 200"
if docker exec "${NAME}" /usr/local/bin/tierplan setup-token >/dev/null 2>&1; then
  fail "setup-token still prints a token after setup is complete"
fi
owners="$(docker cp "${NAME}:/data" - | tar -tvf - | awk '{ print $2 }' | sort -u)"
[ "${owners}" = "1000/1000" ] || fail "files under /data are not all owned by 1000/1000 after the re-run: ${owners}"

echo "IMAGE SMOKE OK (${IMAGE})"
