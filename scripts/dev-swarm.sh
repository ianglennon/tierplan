#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# A local Docker Swarm for the development loop (D-60): three managers and one
# worker, each a privileged docker:<engine>-dind container, with the agent built
# from this source tree loaded on every node. `make dev` is the TierPlan they
# report to.
#
#   scripts/dev-swarm.sh up      create the swarm and load the agent image
#   scripts/dev-swarm.sh down    remove what `up` created
#   scripts/dev-swarm.sh dns     print the DNS servers `up` would give the nodes
#
# `down` removes only the four containers named tierplan-dev-m1..m3 and
# tierplan-dev-w1 (and only when they carry the label this script put on them),
# their anonymous volumes, and the network tierplan-dev-swarm. It never prunes:
# nothing on the machine that this script did not create is touched.
#
# Environment (all optional):
#   DEV_SWARM_ENGINE   dind major version, default 29
#   DEV_SWARM_SUBNET   subnet of the swarm's network, default 172.31.250.0/24.
#                      The first address is the gateway, which is this machine as
#                      the nodes see it; the nodes are .11 to .14.
#   DEV_SWARM_DNS      DNS server IP addresses for every node's inner Docker daemon,
#                      separated by commas or spaces. Default: the host's
#                      non-loopback IPv4 nameservers from /etc/resolv.conf, or from
#                      /run/systemd/resolve/resolv.conf when /etc/resolv.conf lists
#                      only loopback ones such as systemd-resolved's 127.0.0.53.
#                      Why: the nodes sit on a user-defined network, so the inner
#                      daemon sees only Docker's embedded resolver 127.0.0.11,
#                      strips it as loopback and falls back to 8.8.8.8 and 8.8.4.4
#                      for the agents on its default bridge, which cannot resolve
#                      LAN names. Entries must be IP addresses (IPv4 or IPv6) and
#                      not loopback; anything else is refused before anything is
#                      created. Set it when the host has no usable resolver or only
#                      an IPv6 one.
#   DEV_SWARM_RESOLV_CONF, DEV_SWARM_SYSTEMD_RESOLV_CONF
#                      the two files the default is read from; test hooks for
#                      scripts/test-scripts.sh, defaults /etc/resolv.conf and
#                      /run/systemd/resolve/resolv.conf.
#
# Needs Docker (with buildx, for the agent image) and make. Run it through
# `sg docker -c '...'` when your shell predates your docker group membership.
set -euo pipefail

cd "$(dirname "$0")/.."

ENGINE="${DEV_SWARM_ENGINE:-29}"
SUBNET="${DEV_SWARM_SUBNET:-172.31.250.0/24}"
NETWORK=tierplan-dev-swarm
LABEL=tierplan.dev-swarm=true
AGENT_LOCAL=tierplan-agent:dev
AGENT_EDGE=ghcr.io/ianglennon/tierplan-agent:edge
MANAGERS=(tierplan-dev-m1 tierplan-dev-m2 tierplan-dev-m3)
WORKERS=(tierplan-dev-w1)
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

# IPv4 dotted quad, every part 0-255.
is_ipv4() {
  local re='^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$' i
  [[ "$1" =~ ${re} ]] || return 1
  for i in 1 2 3 4; do
    ((10#${BASH_REMATCH[i]} <= 255)) || return 1
  done
}

is_loopback_v4() { [[ "$1" == 127.* ]]; }

# IPv6 literal: hex digits and colons with at least one colon, no zone index. The
# unspecified address and the loopback address (in any written form) do not count.
is_ipv6() {
  [[ "$1" == *:* && "$1" =~ ^[0-9A-Fa-f:]+$ && ! "$1" =~ ^[0:]*:0{0,3}1$ && ! "$1" =~ ^[0:]+$ ]]
}

# nameservers FILE: the nameserver addresses of a resolv.conf; a missing file is empty.
nameservers() {
  [[ -r "$1" ]] || return 0
  awk '$1 == "nameserver" { print $2 }' "$1"
}

# upstream_dns: prints the DNS servers for the nodes' inner daemons, one per line.
# Prints a warning to stderr, and nothing, when there are none.
upstream_dns() {
  local -a parts=() found=()
  local entry file
  if [[ -n "${DEV_SWARM_DNS:-}" ]]; then
    read -ra parts <<<"${DEV_SWARM_DNS//,/ }"
  fi
  if ((${#parts[@]} > 0)); then
    for entry in "${parts[@]}"; do
      if { is_ipv4 "${entry}" && ! is_loopback_v4 "${entry}"; } || is_ipv6 "${entry}"; then
        printf '%s\n' "${entry}"
      else
        echo "dev-swarm: DEV_SWARM_DNS entry '${entry}' is not a non-loopback IP address (use IPv4 or IPv6 literals such as 192.168.1.1)" >&2
        exit 2
      fi
    done
    return 0
  fi
  for file in "${DEV_SWARM_RESOLV_CONF:-/etc/resolv.conf}" "${DEV_SWARM_SYSTEMD_RESOLV_CONF:-/run/systemd/resolve/resolv.conf}"; do
    while IFS= read -r entry; do
      if is_ipv4 "${entry}" && ! is_loopback_v4 "${entry}"; then
        found+=("${entry}")
      fi
    done < <(nameservers "${file}")
    if ((${#found[@]} > 0)); then
      printf '%s\n' "${found[@]}"
      return 0
    fi
  done
  echo "dev-swarm: no non-loopback DNS server found on this host, so agents on the dev swarm will use Docker's fallback 8.8.8.8 and 8.8.4.4, which cannot resolve LAN names; set DEV_SWARM_DNS to your DNS server's address" >&2
}

exists() { docker container inspect "$1" >/dev/null 2>&1; }

all_nodes_ready() {
  local out
  out="$(docker exec tierplan-dev-m1 docker node ls --format '{{.Status}}|{{.ManagerStatus}}')" || return 1
  [[ "$(grep -c '^Ready|' <<<"${out}")" == "${#NODES[@]}" ]] && grep -q '|Leader$' <<<"${out}"
}

up() {
  local n dns_list
  # Resolved first, so a refused DEV_SWARM_DNS stops the script before anything is
  # created (the command substitution carries the exit status under set -e).
  dns_list="$(upstream_dns)"
  local -a dns_servers=() dind_args=()
  if [[ -n "${dns_list}" ]]; then
    mapfile -t dns_servers <<<"${dns_list}"
  fi
  for n in "${dns_servers[@]+"${dns_servers[@]}"}"; do
    dind_args+=(--dns "${n}")
  done
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
    # contacted (the tests do the same). The arguments after the image name reach the
    # inner dockerd: the dind entrypoint prepends dockerd when the first one starts
    # with a dash.
    docker run -d --privileged --name "${n}" --hostname "${n}" \
      --network "${NETWORK}" --ip "$(node_ip "${n}")" \
      --add-host ghcr.io:127.0.0.1 \
      --label "${LABEL}" -e DOCKER_TLS_CERTDIR= \
      "docker:${ENGINE}-dind" "${dind_args[@]}" >/dev/null
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
$(if ((${#dns_servers[@]} > 0)); then echo "The nodes' Docker daemons use DNS server(s): ${dns_servers[*]}"; else echo "No DNS server was found for the nodes: agents there use Docker's fallback 8.8.8.8, which cannot resolve LAN names (set DEV_SWARM_DNS)."; fi)

Next:
  1. Start TierPlan so the nodes can reach it. The nodes see this machine as ${GATEWAY}:
       make dev DEV_LISTEN=0.0.0.0:7480
     (dev-go tells agents to dial wss://${GATEWAY}:7480, and the install command TierPlan
     shows carries the pin of the certificate the server made for itself, so the agents
     verify it. DEV_LISTEN=0.0.0.0:7480 is what lets the nodes reach the server and makes
     it reachable from your network; the setup-token gate still applies, but do not do
     this on a network you do not trust.)
  2. In the UI at https://localhost:5173 choose Environments, Add environment, Agent.
     The browser trusts .devdata/dev-tls/ca.pem once, or click through the warning.
     From another machine add DEV_WEB_HOST=0.0.0.0 DEV_PUBLIC_URL=https://<this machine's name>:5173
     to the make dev command and open that address (see CONTRIBUTING.md).
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
    if [[ "$(docker container inspect -f '{{index .Config.Labels "tierplan.dev-swarm"}}' "${n}")" != "true" ]]; then
      echo "dev-swarm: container ${n} does not carry the ${LABEL} label, so it is not ours; leaving it" >&2
      continue
    fi
    # -v removes the anonymous volume dind keeps /var/lib/docker in.
    docker rm -f -v "${n}" >/dev/null
  done
  if docker network inspect "${NETWORK}" >/dev/null 2>&1; then
    if [[ "$(docker network inspect -f '{{index .Labels "tierplan.dev-swarm"}}' "${NETWORK}")" == "true" ]]; then
      docker network rm "${NETWORK}" >/dev/null
    else
      echo "dev-swarm: network ${NETWORK} does not carry the ${LABEL} label, so it is not ours; leaving it" >&2
    fi
  fi
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  dns) upstream_dns ;;
  *)
    echo "usage: $0 up|down|dns" >&2
    exit 2
    ;;
esac
