#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# A local Docker Swarm for the development loop (D-60): three managers and one
# worker, each a privileged docker:<engine>-dind container, with the agent built
# from this source tree loaded on every node. `make dev` is the Bayplan they
# report to.
#
#   scripts/dev-swarm.sh up      create the swarm and load the agent image
#   scripts/dev-swarm.sh down    remove what `up` created
#
# `down` removes only the four containers named bayplan-dev-m1..m3 and
# bayplan-dev-w1 (and only when they carry the label this script put on them),
# their anonymous volumes, and the network bayplan-dev-swarm. It never prunes:
# nothing on the machine that this script did not create is touched.
#
# Environment (all optional):
#   DEV_SWARM_ENGINE   dind major version, default 29
#   DEV_SWARM_SUBNET   subnet of the swarm's network, default 172.31.250.0/24.
#                      The first address is the gateway, which is this machine as
#                      the nodes see it; the nodes are .11 to .14.
#
# Needs Docker (with buildx, for the agent image) and make. Run it through
# `sg docker -c '...'` when your shell predates your docker group membership.
set -euo pipefail

cd "$(dirname "$0")/.."

ENGINE="${DEV_SWARM_ENGINE:-29}"
SUBNET="${DEV_SWARM_SUBNET:-172.31.250.0/24}"
NETWORK=bayplan-dev-swarm
LABEL=bayplan.dev-swarm=true
AGENT_LOCAL=bayplan-agent:dev
AGENT_EDGE=ghcr.io/ianglennon/bayplan-agent:edge
MANAGERS=(bayplan-dev-m1 bayplan-dev-m2 bayplan-dev-m3)
WORKERS=(bayplan-dev-w1)
NODES=("${MANAGERS[@]}" "${WORKERS[@]}")

# The addresses come from the subnet: only a /24 is supported, so the first three
# octets are the network and the last is the host part.
if [[ ! "${SUBNET}" =~ ^([0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3})\.0/24$ ]]; then
  echo "dev-swarm: DEV_SWARM_SUBNET must be a /24 such as 172.31.250.0/24, not '${SUBNET}'" >&2
  exit 2
fi
PREFIX="${BASH_REMATCH[1]}"
GATEWAY="${PREFIX}.1"

node_ip() {
  local i
  for i in "${!NODES[@]}"; do
    if [[ "${NODES[$i]}" == "$1" ]]; then
      echo "${PREFIX}.$((11 + i))"
      return
    fi
  done
  echo "dev-swarm: unknown node $1" >&2
  return 1
}

# poll DEADLINE_SECONDS WHAT COMMAND...: runs the command once a second until it
# succeeds, or gives up and says what it was waiting for. Never a fixed sleep.
poll() {
  local deadline="$1" what="$2"
  shift 2
  local i
  for ((i = 0; i < deadline; i++)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "dev-swarm: gave up waiting for ${what} after ${deadline}s" >&2
  return 1
}

exists() { docker container inspect "$1" >/dev/null 2>&1; }

all_nodes_ready() {
  local out
  out="$(docker exec bayplan-dev-m1 docker node ls --format '{{.Status}}|{{.ManagerStatus}}')" || return 1
  [[ "$(grep -c '^Ready|' <<<"${out}")" == "${#NODES[@]}" ]] && grep -q '|Leader$' <<<"${out}"
}

up() {
  local n
  for n in "${NODES[@]}"; do
    if exists "${n}"; then
      echo "dev-swarm: container ${n} already exists; run 'make dev-swarm-down' first" >&2
      exit 1
    fi
  done
  if docker network inspect "${NETWORK}" >/dev/null 2>&1; then
    echo "dev-swarm: network ${NETWORK} already exists; run 'make dev-swarm-down' first" >&2
    exit 1
  fi

  # From here on a failure takes back what was created, so a half-built swarm does
  # not sit there holding a subnet.
  trap 'status=$?; if [[ ${status} -ne 0 ]]; then echo "dev-swarm: failed; removing what was created" >&2; down; fi' EXIT

  docker network create --subnet "${SUBNET}" --gateway "${GATEWAY}" --label "${LABEL}" "${NETWORK}" >/dev/null

  for n in "${NODES[@]}"; do
    # ghcr.io resolves to the node itself, so the release image name the install
    # command uses is satisfied by the image loaded below and no registry is ever
    # contacted (the tests do the same).
    docker run -d --privileged --name "${n}" --hostname "${n}" \
      --network "${NETWORK}" --ip "$(node_ip "${n}")" \
      --add-host ghcr.io:127.0.0.1 \
      --label "${LABEL}" -e DOCKER_TLS_CERTDIR= \
      "docker:${ENGINE}-dind" >/dev/null
  done
  for n in "${NODES[@]}"; do
    poll 90 "the engine in ${n}" docker exec "${n}" docker info
  done

  local m1="${MANAGERS[0]}" ip
  ip="$(node_ip "${m1}")"
  docker exec "${m1}" docker swarm init --advertise-addr "${ip}" >/dev/null
  local manager_token worker_token
  manager_token="$(docker exec "${m1}" docker swarm join-token -q manager)"
  worker_token="$(docker exec "${m1}" docker swarm join-token -q worker)"
  for n in "${NODES[@]:1}"; do
    local token="${manager_token}"
    if [[ " ${WORKERS[*]} " == *" ${n} "* ]]; then
      token="${worker_token}"
    fi
    docker exec "${n}" docker swarm join --token "${token}" --advertise-addr "$(node_ip "${n}")" "${ip}:2377" >/dev/null
  done
  poll 90 "a leader and every node ready" all_nodes_ready

  make agent-image
  for n in "${NODES[@]}"; do
    docker save "${AGENT_LOCAL}" | docker exec -i "${n}" docker load >/dev/null
    docker exec "${n}" docker tag "${AGENT_LOCAL}" "${AGENT_EDGE}"
  done

  trap - EXIT
  cat <<EOF

The dev swarm is up: ${#MANAGERS[@]} managers and ${#WORKERS[@]} worker on network ${NETWORK} (${SUBNET}), docker:${ENGINE}-dind.
The agent built from this tree is loaded on every node as ${AGENT_EDGE}.

Next:
  1. Start Bayplan so the nodes can reach it. The nodes see this machine as ${GATEWAY}:
       make dev DEV_LISTEN=0.0.0.0:7480
     (dev-go tells agents to dial http://${GATEWAY}:7480 over plain ws://, which is for
     development only. 0.0.0.0 makes the dev server reachable from your network; the
     setup-token gate still applies, but do not do this on a network you do not trust.)
  2. In the UI at http://localhost:5173 choose Environments, Add environment, Agent.
  3. Paste the swarm command on a manager:
       docker exec -i ${m1} sh
     then paste, press Enter, and press Ctrl-D.

Tear it down with: make dev-swarm-down
EOF
}

down() {
  local n
  for n in "${NODES[@]}"; do
    if ! exists "${n}"; then
      continue
    fi
    if [[ "$(docker container inspect -f '{{index .Config.Labels "bayplan.dev-swarm"}}' "${n}")" != "true" ]]; then
      echo "dev-swarm: container ${n} does not carry the ${LABEL} label, so it is not ours; leaving it" >&2
      continue
    fi
    # -v removes the anonymous volume dind keeps /var/lib/docker in.
    docker rm -f -v "${n}" >/dev/null
  done
  if docker network inspect "${NETWORK}" >/dev/null 2>&1; then
    if [[ "$(docker network inspect -f '{{index .Labels "bayplan.dev-swarm"}}' "${NETWORK}")" == "true" ]]; then
      docker network rm "${NETWORK}" >/dev/null
    else
      echo "dev-swarm: network ${NETWORK} does not carry the ${LABEL} label, so it is not ours; leaving it" >&2
    fi
  fi
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *)
    echo "usage: $0 up|down" >&2
    exit 2
    ;;
esac
