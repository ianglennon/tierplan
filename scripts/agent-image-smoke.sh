#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Runs the agent image (`make agent-image` builds bayplan-agent:dev) with the
# hardening flags the install commands use, and checks what a unit test cannot:
# the process runs as UID and GID 1000 in the Docker socket's group with no
# capability left, no-new-privileges is set even where the platform cannot ask
# for it (a swarm service), the state directory is writable by that user, a
# root-only token file was readable before the drop, and the token is in no log.
#
# Two runs:
#   standalone  token in the environment, --security-opt no-new-privileges
#   swarm-like  token in a root-owned 0400 file (as a Docker secret), and NO
#               --security-opt, so no-new-privileges can only come from the agent
#
# The agent has no server to talk to, so a listener on the host accepts its
# connection and never answers; the agent waits in the dial and stays up while
# its process is inspected. Environment: IMAGE (default bayplan-agent:dev).
# Needs Docker, python3 and tar. Removes every container, volume and process it
# creates.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-bayplan-agent:dev}"
docker image inspect "${IMAGE}" >/dev/null 2>&1 \
  || { echo "AGENT IMAGE SMOKE FAIL: ${IMAGE} not found; run make agent-image first" >&2; exit 1; }

SOCKET=/var/run/docker.sock
[ -S "${SOCKET}" ] || { echo "AGENT IMAGE SMOKE FAIL: ${SOCKET} is not a socket" >&2; exit 1; }
SOCKET_GID="$(stat -c %g "${SOCKET}")"

NAME="bayplan-agent-smoke-$$"
WORK="$(mktemp -d)"
HOLDER_PID=""
VOLUMES=()
CONTAINERS=()

cleanup() {
  local c v
  for c in "${CONTAINERS[@]}"; do docker rm -f "${c}" >/dev/null 2>&1 || true; done
  for v in "${VOLUMES[@]}"; do docker volume rm -f "${v}" >/dev/null 2>&1 || true; done
  [ -z "${HOLDER_PID}" ] || kill "${HOLDER_PID}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

fail() {
  echo "AGENT IMAGE SMOKE FAIL: $1" >&2
  if [ -n "${CURRENT:-}" ]; then docker logs "${CURRENT}" 2>&1 | tail -n 40 >&2 || true; fi
  exit 1
}

# The image itself: no USER line, so it starts as root (the distroless root
# variant reports an empty user or 0) and the binary drops by itself.
user="$(docker image inspect -f '{{.Config.User}}' "${IMAGE}")"
[ -z "${user}" ] || [ "${user}" = 0 ] || [ "${user}" = root ] || fail "the image sets USER '${user}'; the binary must start as root to read a root-only secret and drop itself"

# A listener that accepts connections and never answers.
python3 - "${WORK}/port" "${WORK}/accepted" <<'PY' &
import socket, sys
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 0))
s.listen(8)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
held = []
while True:
    c, _ = s.accept()
    held.append(c)
    open(sys.argv[2], "a").write("x")
PY
HOLDER_PID=$!
for _ in $(seq 1 50); do [ -s "${WORK}/port" ] && break; sleep 0.1; done
[ -s "${WORK}/port" ] || fail "the listener did not start"
PORT="$(cat "${WORK}/port")"
URL="ws://127.0.0.1:${PORT}/agent/connect"

# status_field PID FIELD prints the whole value line of /proc/PID/status.
status_field() { awk -v f="$2:" '$1 == f { $1 = ""; sub(/^ /, ""); print }' "/proc/$1/status"; }

# run_case LABEL TOKEN EXTRA_DOCKER_ARGS... starts the agent, waits until it has
# dialled the listener, checks it and removes it. The state volume is new each
# time, which is the A16 claim: an empty named volume inherits the image's owner.
run_case() {
  local label="$1" token="$2"; shift 2
  local cname="${NAME}-${label}" vol="${NAME}-${label}-state"
  CURRENT="${cname}"
  CONTAINERS+=("${cname}")
  VOLUMES+=("${vol}")
  : >"${WORK}/accepted"

  docker volume create "${vol}" >/dev/null
  docker run -d --name "${cname}" \
    --network host \
    --read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID \
    -v "${SOCKET}:${SOCKET}" \
    -v "${vol}:/var/lib/bayplan-agent" \
    -e "BAYPLAN_AGENT_URL=${URL}" \
    "$@" \
    "${IMAGE}" >/dev/null || fail "${label}: docker run failed"

  for _ in $(seq 1 100); do
    [ -s "${WORK}/accepted" ] && break
    [ "$(docker inspect -f '{{.State.Running}}' "${cname}")" = "true" ] || fail "${label}: the agent exited while starting"
    sleep 0.2
  done
  [ -s "${WORK}/accepted" ] || fail "${label}: the agent never dialled the listener"

  local pid
  pid="$(docker inspect -f '{{.State.Pid}}' "${cname}")"
  [ "${pid}" != 0 ] || fail "${label}: no process id"

  # 1. UID and GID 1000 in the real, effective, saved and file-system ids.
  [ "$(status_field "${pid}" Uid)" = "1000 1000 1000 1000" ] || fail "${label}: Uid is '$(status_field "${pid}" Uid)', want 1000 in all four"
  [ "$(status_field "${pid}" Gid)" = "1000 1000 1000 1000" ] || fail "${label}: Gid is '$(status_field "${pid}" Gid)', want 1000 in all four"

  # 2. Only the Docker socket's group is added, and the capabilities are gone.
  [ "$(status_field "${pid}" Groups)" = "${SOCKET_GID}" ] || fail "${label}: supplementary groups are '$(status_field "${pid}" Groups)', want only ${SOCKET_GID}"
  [ "$(status_field "${pid}" CapEff)" = "0000000000000000" ] || fail "${label}: CapEff is '$(status_field "${pid}" CapEff)', want none after the drop"

  # 3. no-new-privileges, set by the agent itself (RESEARCH A9).
  [ "$(status_field "${pid}" NoNewPrivs)" = "1" ] || fail "${label}: NoNewPrivs is '$(status_field "${pid}" NoNewPrivs)', want 1"

  # 4. The dropped user wrote the start marker in the state volume, and the
  # volume belongs to 1000 (docker cp shows owners as uid/gid).
  local listing
  listing="$(docker cp "${cname}:/var/lib/bayplan-agent" - | tar -tvf -)"
  printf '%s\n' "${listing}" | awk '$6 ~ /\/started$/ { found = 1; if ($2 != "1000/1000") bad = 1 } END { exit !(found && !bad) }' \
    || fail "${label}: no start marker owned by 1000/1000 in the state volume:
${listing}"
  printf '%s\n' "${listing}" | awk '$6 ~ /^bayplan-agent\/$/ { found = 1; if ($2 != "1000/1000" || $1 !~ /^drwx------/) bad = 1 } END { exit !(found && !bad) }' \
    || fail "${label}: the state directory is not 1000/1000 mode 0700:
${listing}"

  # 5. The log names the server's host and carries no token, and the engine
  # answered over the socket (the agent warns when GET /info fails).
  local logs
  logs="$(docker logs "${cname}" 2>&1)"
  printf '%s' "${logs}" | grep -q 'starting bayplan-agent' || fail "${label}: no start line in the log"
  printf '%s' "${logs}" | grep -q "server=127.0.0.1:${PORT}" || fail "${label}: the start line does not name the server host"
  printf '%s' "${logs}" | grep -q 'uid=1000' || fail "${label}: the start line does not report uid=1000"
  printf '%s' "${logs}" | grep -q 'token_present=true' || fail "${label}: the agent did not find its token"
  if printf '%s' "${logs}" | grep -qF -- "${token}"; then fail "${label}: the token is in the log"; fi
  if printf '%s' "${logs}" | grep -q 'did not answer GET /info'; then fail "${label}: the dropped user cannot reach the Docker socket"; fi

  echo "agent runs as UID 1000 (${label}): groups ${SOCKET_GID}, no capabilities, no-new-privileges, state volume writable, token not logged"
  docker rm -f "${cname}" >/dev/null
  CURRENT=""
}

# --- standalone: token in the environment, the engine asked for no-new-privileges.
TOKEN_ENV="bpjt_smoke_ENVSECRET_$$"
run_case standalone "${TOKEN_ENV}" \
  --security-opt no-new-privileges \
  -e "BAYPLAN_AGENT_JOIN_TOKEN=${TOKEN_ENV}"

# --- swarm-like: the token is a root-owned 0400 file on a volume (a Docker
# secret is mounted that way), and the engine is NOT asked for no-new-privileges.
TOKEN_FILE="bpjt_smoke_FILESECRET_$$"
SECVOL="${NAME}-secret"
SECBOX="${NAME}-secretbox"
VOLUMES+=("${SECVOL}")
CONTAINERS+=("${SECBOX}")
docker volume create "${SECVOL}" >/dev/null
docker create --name "${SECBOX}" -v "${SECVOL}:/run/secrets" "${IMAGE}" >/dev/null
printf '%s\n' "${TOKEN_FILE}" >"${WORK}/bayplan-agent-token"
tar -C "${WORK}" --owner=0 --group=0 --mode=0400 -cf - bayplan-agent-token | docker cp - "${SECBOX}:/run/secrets/" \
  || fail "could not place the secret file"
docker cp "${SECBOX}:/run/secrets" - | tar -tvf - | awk '$6 ~ /bayplan-agent-token$/ { found = 1; if ($2 != "0/0" || $1 != "-r--------") bad = 1 } END { exit !(found && !bad) }' \
  || fail "the secret file is not root-owned 0400"
docker rm -f "${SECBOX}" >/dev/null

run_case swarm-like "${TOKEN_FILE}" \
  -v "${SECVOL}:/run/secrets:ro" \
  -e BAYPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/bayplan-agent-token

echo "AGENT IMAGE SMOKE OK (${IMAGE}): the agent runs as UID 1000 under both install styles"
