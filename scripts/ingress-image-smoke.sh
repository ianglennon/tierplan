#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Runs the ingress image (`make ingress-image` builds tierplan-ingress:dev) the way
# a node runs it and checks what a unit test cannot (D-05, D-06, D-13, PRX-05, PRX-06):
#
#   - the process is UID and GID 1000 with no capability and no-new-privileges set,
#     with every hardening flag (--read-only, --cap-drop ALL, no-new-privileges) and
#     with none of them, as a swarm service runs it (`docker service create` cannot
#     ask for them, so the binary sets no-new-privileges itself);
#   - nothing is added (HostConfig.CapAdd is empty) and no Docker socket is mounted;
#   - a new named volume and the directories the supervisor creates in it are owned
#     by 1000 and private (mode 0700), so no chown is needed;
#   - with no cache and a TierPlan that cannot be reached the node serves nothing:
#     the readiness endpoint answers 503 with an empty body, the proxy ports 8080
#     and 8443 refuse connections, the health status is "starting", the log says
#     there is no verified cached bundle, and the join token is in no log.
#
# The TierPlan address is a closed port on the container's own loopback, so the
# supervisor's dial is refused at once and it backs off. The token is a throwaway
# random value. Environment: IMAGE (default tierplan-ingress:dev). Needs Docker,
# curl and tar, and a host that can reach a container's bridge address. Removes every
# container, volume and file it creates; they carry the label tierplan.ci.owner
# (TIERPLAN_CI_OWNER, local-<pid> when unset) so `make ci-clean` removes what a
# cancelled CI job left (D-23), and their names carry this process id, so two runs on
# one runner do not collide.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-tierplan-ingress:dev}"
docker image inspect "${IMAGE}" >/dev/null 2>&1 \
  || { echo "INGRESS IMAGE SMOKE FAIL: ${IMAGE} not found; run make ingress-image first" >&2; exit 1; }

NAME="tierplan-ingress-smoke-$$"
OWNER_LABEL="tierplan.ci.owner=${TIERPLAN_CI_OWNER:-local-$$}"
WORK="$(mktemp -d)"
VOLUMES=()
CONTAINERS=()
CURRENT=""

cleanup() {
  local c v
  for c in "${CONTAINERS[@]}"; do docker rm -f "${c}" >/dev/null 2>&1 || true; done
  for v in "${VOLUMES[@]}"; do docker volume rm -f "${v}" >/dev/null 2>&1 || true; done
  rm -rf "${WORK}"
}
trap cleanup EXIT

# fail prints the reason and the last 40 log lines of the container under test. The
# token is a throwaway random value and the supervisor never logs it; a log that does
# is itself a failure, handled where it is checked and without printing the line.
fail() {
  echo "INGRESS IMAGE SMOKE FAIL: $1" >&2
  if [ -n "${CURRENT}" ]; then docker logs "${CURRENT}" 2>&1 | tail -n 40 >&2 || true; fi
  exit 1
}

pass() { echo "PASS: $1"; }

# status_field PID FIELD prints the whole value line of /proc/PID/status.
status_field() { awk -v f="$2:" '$1 == f { $1 = ""; sub(/^ /, ""); print }' "/proc/$1/status"; }

# 1. The image config.
user="$(docker image inspect -f '{{.Config.User}}' "${IMAGE}")"
[ "${user}" = "1000:1000" ] || fail "the image user is '${user}', want 1000:1000"
pass "the image config sets User 1000:1000"

# A token file the container reads, as it would read a Docker secret.
TOKEN="tpit_smoke_$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
printf '%s\n' "${TOKEN}" >"${WORK}/token"
chmod 0444 "${WORK}/token"

# start_case LABEL EXTRA_DOCKER_ARGS... starts a node on a new volume and sets
# CURRENT, PID and IP. Nothing it starts holds the Docker socket.
start_case() {
  local label="$1"; shift
  local cname="${NAME}-${label}" vol="${NAME}-${label}-data"
  CURRENT="${cname}"
  CONTAINERS+=("${cname}")
  VOLUMES+=("${vol}")
  docker volume create --label "${OWNER_LABEL}" "${vol}" >/dev/null
  docker run -d --name "${cname}" --label "${OWNER_LABEL}" \
    "$@" \
    -v "${vol}:/var/lib/tierplan-ingress" \
    --mount "type=bind,src=${WORK}/token,dst=/run/secrets/ingress-join-token,readonly" \
    -e TIERPLAN_INGRESS_URL=wss://127.0.0.1:9/ingress/connect \
    -e TIERPLAN_INGRESS_JOIN_TOKEN_FILE=/run/secrets/ingress-join-token \
    "${IMAGE}" >/dev/null || fail "${label}: docker run failed"

  # Poll in 1-second steps for up to 60 seconds until the container answers.
  PID=""
  for _ in $(seq 1 60); do
    [ "$(docker inspect -f '{{.State.Running}}' "${cname}")" = "true" ] || fail "${label}: the node exited while starting"
    PID="$(docker inspect -f '{{.State.Pid}}' "${cname}")"
    IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${cname}")"
    [ "${PID}" != 0 ] && [ -n "${IP}" ] && break
    sleep 1
  done
  [ -n "${PID}" ] && [ "${PID}" != 0 ] || fail "${label}: no process id"
  [ -n "${IP}" ] || fail "${label}: no container address"
}

# check_process LABEL checks the identity of the process in CURRENT (PID is its host pid).
check_process() {
  local label="$1"
  [ "$(status_field "${PID}" Uid)" = "1000 1000 1000 1000" ] || fail "${label}: Uid is '$(status_field "${PID}" Uid)', want 1000 in all four"
  [ "$(status_field "${PID}" Gid)" = "1000 1000 1000 1000" ] || fail "${label}: Gid is '$(status_field "${PID}" Gid)', want 1000 in all four"
  [ "$(status_field "${PID}" CapEff)" = "0000000000000000" ] || fail "${label}: CapEff is '$(status_field "${PID}" CapEff)', want none"
  [ "$(status_field "${PID}" NoNewPrivs)" = "1" ] || fail "${label}: NoNewPrivs is '$(status_field "${PID}" NoNewPrivs)', want 1"
}

# --- Run A: every hardening flag.
start_case hardened --read-only --cap-drop ALL --security-opt no-new-privileges
cname="${CURRENT}"

# wait_503 LABEL polls in 1-second steps, for up to 60 seconds, until the readiness endpoint
# of CURRENT answers 503. By then the supervisor is running and has set no-new-privileges.
wait_503() {
  local label="$1" code=""
  for _ in $(seq 1 60); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${IP}:8083/readyz" || true)"
    [ "${code}" = 503 ] && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "${CURRENT}")" = "true" ] || fail "${label}: the node exited while waiting for readiness"
    sleep 1
  done
  fail "${label}: /readyz answered '${code}', want 503"
}

# 2. With no cache and no TierPlan: not ready, serving nothing.
wait_503 hardened
body="$(curl -s --max-time 2 "http://${IP}:8083/readyz" || true)"
[ -z "${body}" ] || fail "hardened: the 503 has a body of ${#body} bytes, want none"
pass "no cache and no TierPlan: /readyz answers 503 with an empty body"

for port in 8080 8443; do
  rc=0
  curl -s -o /dev/null --max-time 2 "http://${IP}:${port}/" || rc=$?
  [ "${rc}" = 7 ] || fail "hardened: a connection to ${port} ended with curl exit ${rc}, want 7 (refused)"
done
pass "ports 8080 and 8443 refuse connections: no route is served"

health="$(docker inspect -f '{{.State.Health.Status}}' "${cname}")"
[ "${health}" = starting ] || fail "hardened: health status is '${health}', want starting"
pass "the container's health status is starting (the 10 minute start period holds)"

check_process hardened
pass "hardened run: UID and GID 1000, CapEff 0, NoNewPrivs 1"

capadd="$(docker inspect -f '{{json .HostConfig.CapAdd}}' "${cname}")"
{ [ "${capadd}" = null ] || [ "${capadd}" = "[]" ]; } || fail "hardened: HostConfig.CapAdd is ${capadd}, want none"
mounts="$(docker inspect -f '{{range .Mounts}}{{.Source}} {{.Destination}}{{"\n"}}{{end}}' "${cname}")"
if printf '%s' "${mounts}" | grep -q 'docker\.sock'; then fail "hardened: a Docker socket is mounted"; fi
if docker inspect -f '{{range .HostConfig.Binds}}{{.}}{{"\n"}}{{end}}' "${cname}" | grep -q 'docker\.sock'; then fail "hardened: a Docker socket is bound"; fi
pass "no added capability and no Docker socket mount"

logs="$(docker logs "${cname}" 2>&1)"
printf '%s' "${logs}" | grep -q 'no verified cached bundle' || fail "hardened: the log has no 'no verified cached bundle' line"
if printf '%s' "${logs}" | grep -qF -- "${TOKEN}"; then
  echo "FAIL: the join token is in the log" >&2
  CURRENT=""
  exit 1
fi
pass "the log says there is no verified cached bundle and holds no join token"

# 3. The volume: the root and what the supervisor created in it belong to 1000, private.
listing="$(docker cp "${cname}:/var/lib/tierplan-ingress" - | tar -tvf -)"
for d in "" dynamic certs state tmp static; do
  printf '%s\n' "${listing}" | awk -v want="tierplan-ingress/${d:+${d}/}" '$6 == want { found = 1; if ($2 != "1000/1000" || $1 !~ /^drwx------/) bad = 1 } END { exit !(found && !bad) }' \
    || fail "hardened: ${d:-the volume root} is not 1000/1000 mode 0700:
${listing}"
done
pass "a new named volume and its root, dynamic, certs, state, tmp and static are 1000/1000 mode 0700"

docker rm -f "${cname}" >/dev/null
CURRENT=""

# --- Run B: none of --read-only, --cap-drop and --security-opt, as a swarm service runs it.
start_case plain
wait_503 plain
check_process plain
capadd="$(docker inspect -f '{{json .HostConfig.CapAdd}}' "${CURRENT}")"
{ [ "${capadd}" = null ] || [ "${capadd}" = "[]" ]; } || fail "plain: HostConfig.CapAdd is ${capadd}, want none"
pass "plain run (no hardening flags): UID and GID 1000, CapEff 0, NoNewPrivs 1 set by the binary"

docker rm -f "${CURRENT}" >/dev/null
CURRENT=""

echo "INGRESS IMAGE SMOKE PASS"
