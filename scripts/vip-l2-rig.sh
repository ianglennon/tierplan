#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Spike S3 leg 2 and the virtual IP's UAT rig (D-01, A-10): a throwaway three-node
# swarm whose nodes sit on the homelab LAN through a macvlan network, the keepalived
# image holding one free LAN address, read from another machine on the same switch.
# What leg 2 proves is what a bridge cannot: that the gratuitous ARP and the VRRP
# adverts of a holder cross a real switch, so a LAN neighbour's ARP cache follows the
# address to the next holder within seconds.
#
# This rig never touches the persistent dev swarm. Everything it makes is named
# tp-l2-* (three containers, one macvlan network) and carries the label
# tierplan.l2-rig=true; `down` removes only that, and never prunes.
#
# It needs three inputs from the maintainer (they cannot be found from here):
#   VIP_L2_ADDRESS   the free LAN address with its prefix, outside DHCP: 192.168.1.240/24
#   VIP_L2_RANGE     a free range of at least four addresses, outside DHCP, inside the
#                    LAN prefix and not holding the address: 192.168.1.224/28
#   VIP_L2_PARENT    this machine's LAN interface that carries the macvlan network: eth0
#   VIP_L2_OBSERVER  an ssh target on the same LAN that is not this machine (a macvlan
#                    parent cannot reach its own children): build01
# and, optionally:
#   VIP_L2_GATEWAY   the LAN gateway, default the .1 of the prefix
#   VIP_L2_VRID      the virtual router ID, default 51 (as leg 1)
#   VIP_L2_ENGINE    the dind major version, default 29
# The observer needs key login, and `sudo -n` for `ip neigh flush` (without it the
# neighbour entry it reads can be stale). Every VIP_L2_* above except the optional
# ones is required, except for `plan`, which fills placeholders.
#
#   scripts/vip-l2-rig.sh plan            print every command the rig would run, run none
#   scripts/vip-l2-rig.sh check-free      ask the observer that the address and the node addresses are unused
#   scripts/vip-l2-rig.sh up              make the network, the swarm, the readiness stand-in and keepalived
#   scripts/vip-l2-rig.sh holder          each node's address, MAC, whether it holds the address, and its state
#   scripts/vip-l2-rig.sh observe         ping the address from the observer and say which node's MAC answers
#   scripts/vip-l2-rig.sh wait-holder     wait until exactly one node holds the address
#   scripts/vip-l2-rig.sh fail-probe N    take node N's readiness stand-in away so its health check fails
#   scripts/vip-l2-rig.sh kill-holder     SIGKILL the holder's keepalived container
#   scripts/vip-l2-rig.sh wait-move N     poll the observer until another node than N answers, print the seconds
#   scripts/vip-l2-rig.sh restore         give every node its readiness stand-in back
#   scripts/vip-l2-rig.sh down            remove everything `up` made
#
# Nodes are named tp-l2-n1..n3; N may be given as n2 or tp-l2-n2. Needs Docker, ssh, and
# the image tierplan-keepalived:dev (`make keepalived-image`) on this machine.
set -euo pipefail

# fd 3 is where the commands are printed, so a command substitution never captures them.
exec 3>&1

DRY=0
KA_IMAGE=tierplan-keepalived:dev
WHOAMI_IMAGE=traefik/whoami:v1.10
NET=tp-l2-net
RIG_LABEL=tierplan.l2-rig=true
NODES=(tp-l2-n1 tp-l2-n2 tp-l2-n3)
SVC=tp-l2-keepalived
READY=tp-l2-ready
READY_LABEL=s3ready
READY_PORT=8083
CONF_NAME=tp-l2-conf
SECRET_NAME=tp-l2-key
SECRET_TARGET=l2-key
ENGINE="${VIP_L2_ENGINE:-29}"
VRID="${VIP_L2_VRID:-51}"

say() { printf '+ %s\n' "$*" >&3; }
note() { printf '# %s\n' "$*" >&3; }
die() {
  echo "vip-l2-rig: $*" >&2
  exit 1
}

# run prints a command and, unless this is a plan, runs it.
run() {
  say "$*"
  if ((DRY)); then return 0; fi
  "$@"
}

# runsh is run for a pipeline or a redirection, given as one string.
runsh() {
  say "$1"
  if ((DRY)); then return 0; fi
  bash -c "$1"
}

# poll SECONDS WHAT COMMAND...: runs the command once a second until it succeeds, or gives up and says what it waited for.
poll() {
  local deadline="$1" what="$2" i
  shift 2
  if ((DRY)); then
    note "wait up to ${deadline}s for ${what}: $*"
    return 0
  fi
  for ((i = 0; i < deadline; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  die "gave up waiting for ${what} after ${deadline}s"
}

ip2int() {
  local a b c d IFS=.
  read -r a b c d <<<"$1"
  echo $(((a << 24) | (b << 16) | (c << 8) | d))
}
int2ip() { echo "$((($1 >> 24) & 255)).$((($1 >> 16) & 255)).$((($1 >> 8) & 255)).$(($1 & 255))"; }

is_ipv4() {
  local re='^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$' i
  [[ "$1" =~ ${re} ]] || return 1
  for i in 1 2 3 4; do
    ((10#${BASH_REMATCH[i]} <= 255)) || return 1
  done
}

# cidr ADDR/BITS: sets C_ADDR, C_BITS, C_INT (the address) and C_NET (the network address) or dies naming WHAT.
cidr() {
  local value="$1" what="$2"
  [[ "${value}" =~ ^([0-9.]+)/([0-9]{1,2})$ ]] || die "${what} must be an IPv4 address with its prefix length such as 192.168.1.240/24, not '${value}'"
  C_ADDR="${BASH_REMATCH[1]}"
  C_BITS="${BASH_REMATCH[2]}"
  is_ipv4 "${C_ADDR}" || die "${what} '${value}' is not an IPv4 address"
  ((C_BITS >= 8 && C_BITS <= 30)) || die "${what} '${value}' has a prefix length outside 8 to 30"
  C_INT=$(ip2int "${C_ADDR}")
  C_NET=$((C_INT & (0xFFFFFFFF << (32 - C_BITS) & 0xFFFFFFFF)))
}

# configure reads and checks the inputs and derives the addresses. In a plan, a missing input is a placeholder.
configure() {
  local need=1
  if ((DRY)); then need=0; fi
  local address="${VIP_L2_ADDRESS:-}" range="${VIP_L2_RANGE:-}"
  PARENT="${VIP_L2_PARENT:-}"
  OBSERVER="${VIP_L2_OBSERVER:-}"
  if ((need)); then
    for v in VIP_L2_ADDRESS VIP_L2_RANGE VIP_L2_PARENT VIP_L2_OBSERVER; do
      [[ -n "${!v:-}" ]] || die "${v} is required (see the header of this script)"
    done
  else
    : "${address:=192.168.1.240/24}" "${range:=192.168.1.224/28}" "${PARENT:=eth0}" "${OBSERVER:=observer-host}"
  fi
  cidr "${address}" VIP_L2_ADDRESS
  VIP="${C_ADDR}"
  VIP_BITS="${C_BITS}"
  VIP_INT="${C_INT}"
  LAN_NET="${C_NET}"
  SUBNET="$(int2ip "${LAN_NET}")/${VIP_BITS}"
  cidr "${range}" VIP_L2_RANGE
  local range_int="${C_NET}" range_size=$((1 << (32 - C_BITS)))
  ((C_BITS >= VIP_BITS && (range_int & (0xFFFFFFFF << (32 - VIP_BITS) & 0xFFFFFFFF)) == LAN_NET)) ||
    die "VIP_L2_RANGE ${range} is not inside the LAN prefix ${SUBNET}"
  ((range_size >= 4)) || die "VIP_L2_RANGE ${range} holds fewer than four addresses"
  RANGE="$(int2ip "${range_int}")/${C_BITS}"
  ((VIP_INT < range_int || VIP_INT >= range_int + range_size)) || die "the address ${VIP} lies inside the range ${RANGE}"
  if [[ -n "${VIP_L2_GATEWAY:-}" ]]; then
    is_ipv4 "${VIP_L2_GATEWAY}" || die "VIP_L2_GATEWAY '${VIP_L2_GATEWAY}' is not an IPv4 address"
    GATEWAY="${VIP_L2_GATEWAY}"
  else
    GATEWAY="$(int2ip $((LAN_NET + 1)))"
  fi
  local gw_int
  gw_int=$(ip2int "${GATEWAY}")
  ((gw_int < range_int || gw_int >= range_int + range_size)) || die "the gateway ${GATEWAY} lies inside the range ${RANGE}"
  # The three nodes take the second to the fourth address of the range (the first may be the range's own network address).
  NODE_IPS=("$(int2ip $((range_int + 1)))" "$(int2ip $((range_int + 2)))" "$(int2ip $((range_int + 3)))")
  [[ "${VRID}" =~ ^[0-9]+$ ]] && ((VRID >= 1 && VRID <= 255)) || die "VIP_L2_VRID '${VRID}' is not 1 to 255"
}

node_of() { # node_of N: n2 or tp-l2-n2 to tp-l2-n2
  local n="$1" x
  [[ "${n}" == tp-l2-* ]] || n="tp-l2-${n}"
  for x in "${NODES[@]}"; do
    [[ "${x}" == "${n}" ]] && {
      echo "${n}"
      return
    }
  done
  die "unknown node '$1' (one of: ${NODES[*]})"
}

node_ip() {
  local i
  for i in "${!NODES[@]}"; do
    [[ "${NODES[$i]}" == "$1" ]] && {
      echo "${NODE_IPS[$i]}"
      return
    }
  done
}

# nx NODE COMMAND...: a command in the node's own engine namespace, printed and run.
nx() {
  local n="$1"
  shift
  run docker exec "${n}" "$@"
}

# nq NODE COMMAND...: the same for a read whose output the caller wants; printed on fd 3, empty in a plan.
nq() {
  local n="$1"
  shift
  say docker exec "${n}" "$@"
  if ((DRY)); then return 0; fi
  docker exec "${n}" "$@"
}

# node_iface NODE: the interface of the node that carries its macvlan address.
node_iface() {
  local ip
  ip="$(node_ip "$1")"
  if ((DRY)); then
    say "docker exec $1 ip -4 -o addr show  # the interface that carries ${ip}"
    echo "eth0"
    return
  fi
  docker exec "$1" ip -4 -o addr show | awk -v a="${ip}" '{ split($4, p, "/"); if (p[1] == a) { print $2; exit } }'
}

node_mac() { # node_mac NODE IFACE
  if ((DRY)); then
    say "docker exec $1 cat /sys/class/net/$2/address"
    echo "<mac of $1>"
    return
  fi
  docker exec "$1" cat "/sys/class/net/$2/address"
}

# holds NODE: succeeds when the node lists the address on any interface.
holds() {
  docker exec "$1" ip -4 -o addr show 2>/dev/null | grep -q "inet ${VIP}/"
}

holders() {
  local n out=()
  for n in "${NODES[@]}"; do
    if holds "${n}"; then out+=("${n}"); fi
  done
  echo "${out[*]:-}"
}

one_holder() {
  local h
  h="$(holders)"
  [[ -n "${h}" && "${h}" != *" "* ]]
}

# The keepalived configuration of leg 1 (internal/dockerx/s3_harness_integration_test.go s3Conf), for this interface and address.
conf() {
  cat <<EOF
global_defs {
    enable_script_security
    script_user root
    vrrp_garp_master_refresh 60
}
vrrp_script chk_proxy {
    script "/usr/local/bin/tierplan-ingress healthcheck"
    interval 1
    timeout 1
    fall 2
    rise 2
}
vrrp_instance TP {
    state BACKUP
    interface $1
    virtual_router_id ${VRID}
    priority 100
    advert_int 1
    nopreempt
    auth_hmac {
        key 1 file:/run/secrets/${SECRET_TARGET}
        active_key 1
        mode enforce
        anti_replay time
        time_window 30
    }
    virtual_ipaddress {
        ${VIP}/${VIP_BITS} dev $1
    }
    track_script { chk_proxy }
}
EOF
}

# observer_script: what the observer runs for each address given: neither a ping nor an ARP answer means free.
observer_check() {
  local addrs=("$@") remote
  remote='used=0; for a in "$@"; do sudo -n ip neigh flush to "$a" >/dev/null 2>&1; if ping -c2 -W1 "$a" >/dev/null 2>&1; then echo "$a answers ping"; used=1; fi; if ip neigh show "$a" | grep -q lladdr; then echo "$a has an ARP answer: $(ip neigh show "$a")"; used=1; fi; done; exit $used'
  say "ssh ${OBSERVER} bash -c '<each address: flush, ping -c2, ip neigh show>' -- ${addrs[*]}"
  if ((DRY)); then return 0; fi
  ssh -o BatchMode=yes "${OBSERVER}" bash -c "'${remote}'" -- "${addrs[@]}"
}

check_free() {
  observer_check "${VIP}" "${NODE_IPS[@]}" || die "an address of the rig answers on the LAN (above); choose another and start again"
  note "the address ${VIP} and the node addresses ${NODE_IPS[*]} answer neither ping nor ARP from ${OBSERVER}"
}

up() {
  local n i
  note "inputs: address ${VIP}/${VIP_BITS}, subnet ${SUBNET}, gateway ${GATEWAY}, range ${RANGE}, parent ${PARENT}, observer ${OBSERVER}, VRID ${VRID}"
  if ! ((DRY)); then
    for n in "${NODES[@]}"; do
      if docker container inspect "${n}" >/dev/null 2>&1; then die "container ${n} already exists; run '$0 down' first"; fi
    done
    if docker network inspect "${NET}" >/dev/null 2>&1; then die "network ${NET} already exists; run '$0 down' first"; fi
    for i in "${KA_IMAGE}" "${WHOAMI_IMAGE}" "docker:${ENGINE}-dind"; do
      docker image inspect "${i}" >/dev/null 2>&1 || die "image ${i} is not on this machine"
    done
  fi
  check_free

  # From here on a failure takes back what was made.
  if ! ((DRY)); then
    trap 'status=$?; if [[ ${status} -ne 0 ]]; then echo "vip-l2-rig: failed; removing what was made" >&2; down; fi' EXIT
  fi

  KEY="hex:$(if ((DRY)); then echo '<64 hex digits>'; else head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; fi)"
  export KEY
  note "the key is 32 random bytes made now, held only in the swarm secret ${SECRET_NAME} and printed nowhere"

  run docker network create -d macvlan --subnet "${SUBNET}" --gateway "${GATEWAY}" --ip-range "${RANGE}" -o "parent=${PARENT}" --label "${RIG_LABEL}" "${NET}"
  for i in "${!NODES[@]}"; do
    n="${NODES[$i]}"
    run docker run -d --privileged --name "${n}" --hostname "${n}" --network "${NET}" --ip "${NODE_IPS[$i]}" \
      --label "${RIG_LABEL}" -e DOCKER_TLS_CERTDIR= "docker:${ENGINE}-dind"
  done
  for n in "${NODES[@]}"; do
    poll 90 "the engine in ${n}" docker exec "${n}" docker info
  done

  local m1="${NODES[0]}" wtoken
  nx "${m1}" docker swarm init --advertise-addr "${NODE_IPS[0]}"
  if ((DRY)); then wtoken="<worker token>"; else wtoken="$(docker exec "${m1}" docker swarm join-token -q worker)"; fi
  say "docker exec ${m1} docker swarm join-token -q worker  # the token for the two joins"
  for i in 1 2; do
    nx "${NODES[$i]}" docker swarm join --token "${wtoken}" --advertise-addr "${NODE_IPS[$i]}" "${NODE_IPS[0]}:2377"
  done
  poll 90 "every node ready" bash -c "[[ \$(docker exec ${m1} docker node ls --format '{{.Status}}' | grep -c '^Ready') == 3 ]]"

  for n in "${NODES[@]}"; do
    runsh "docker save ${KA_IMAGE} | docker exec -i ${n} docker load"
    runsh "docker save ${WHOAMI_IMAGE} | docker exec -i ${n} docker load"
    nx "${m1}" docker node update --label-add "${READY_LABEL}=yes" "${n}"
  done

  # The readiness stand-in: whoami answers 200 on 8083 where the label says the node is ready; the check asks 127.0.0.1:8083/readyz.
  nx "${m1}" docker service create --name "${READY}" --mode global --no-resolve-image \
    --constraint "node.labels.${READY_LABEL}==yes" --publish "mode=host,target=${READY_PORT},published=${READY_PORT}" \
    --stop-grace-period 1s "${WHOAMI_IMAGE}" --port "${READY_PORT}"
  poll 120 "the readiness stand-in on every node" bash -c "[[ \$(docker exec ${m1} docker service ls --filter name=${READY} --format '{{.Replicas}}') == 3/3 ]]"

  local iface conf_file
  iface="$(node_iface "${m1}")"
  [[ -n "${iface}" ]] || die "no interface of ${m1} carries ${NODE_IPS[0]}"
  note "the parent-facing interface inside the nodes is ${iface}"
  conf_file="$(mktemp)"
  conf "${iface}" >"${conf_file}"
  note "keepalived.conf, the swarm config ${CONF_NAME}:"
  if ((DRY)); then sed 's/^/#   /' "${conf_file}" >&3; fi
  runsh "docker exec -i ${m1} docker config create ${CONF_NAME} - < ${conf_file}"
  runsh "printf '%s' \"\$KEY\" | docker exec -i ${m1} docker secret create ${SECRET_NAME} -"
  nx "${m1}" docker service create --name "${SVC}" --mode global --no-resolve-image --network host \
    --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW --cap-add SETUID --cap-add SETGID --read-only \
    --mount type=tmpfs,target=/run --mount type=tmpfs,target=/tmp \
    --secret "source=${SECRET_NAME},target=${SECRET_TARGET},mode=0400" \
    --config "source=${CONF_NAME},target=/etc/keepalived/keepalived.conf,mode=0444" \
    --stop-grace-period 10s --entrypoint /usr/sbin/keepalived "${KA_IMAGE}" \
    --dont-fork --log-console --log-detail --use-file /etc/keepalived/keepalived.conf
  rm -f "${conf_file}"
  poll 120 "the keepalived task on every node" bash -c "[[ \$(docker exec ${m1} docker service ls --filter name=${SVC} --format '{{.Replicas}}') == 3/3 ]]"
  poll 60 "exactly one holder of ${VIP}" one_holder
  trap - EXIT
  note "up: the rig is running; ${VIP} is held by: $(if ((DRY)); then echo '<holder>'; else holders; fi)"
}

# state_line NODE: the last state keepalived of the node's newest task container logged, and whether it ever logged MASTER.
state_line() {
  local n="$1" id last masters
  id="$(docker exec "${n}" docker ps -q --filter "label=com.docker.swarm.service.name=${SVC}" 2>/dev/null | head -n 1)"
  [[ -n "${id}" ]] || {
    echo "no running task"
    return
  }
  last="$(docker exec "${n}" docker logs "${id}" 2>&1 | grep -oE 'Entering [A-Z]+ STATE' | tail -n 1 || true)"
  masters="$(docker exec "${n}" docker logs "${id}" 2>&1 | grep -c 'Entering MASTER STATE' || true)"
  echo "task ${id:0:12}: ${last:-no state yet} (MASTER entered ${masters}x in this task)"
}

holder() {
  local n iface mac held
  for n in "${NODES[@]}"; do
    if ((DRY)); then
      say "docker exec ${n} ip -4 -o addr show  # does it list ${VIP}"
      say "docker exec ${n} cat /sys/class/net/<iface>/address"
      say "docker exec ${n} docker ps -q / docker logs  # its last keepalived state"
      continue
    fi
    iface="$(node_iface "${n}")"
    mac="$(node_mac "${n}" "${iface}")"
    held=no
    if holds "${n}"; then held=YES; fi
    printf '%s addr=%s %s mac=%s holds-%s=%s state: %s\n' "${n}" "$(node_ip "${n}")" "${iface}" "${mac}" "${VIP}" "${held}" "$(state_line "${n}")"
  done
}

# observe_mac: the MAC the observer has for the address after a ping, empty when none answered.
observe_mac() {
  local remote="sudo -n ip neigh flush to ${VIP} >/dev/null 2>&1; ping -c1 -W1 ${VIP} >/dev/null 2>&1; ip neigh show ${VIP}"
  if ((DRY)); then
    say "ssh ${OBSERVER} '${remote}'"
    return 0
  fi
  ssh -o BatchMode=yes "${OBSERVER}" "${remote}" | awk '{ for (i = 1; i < NF; i++) if ($i == "lladdr") { print $(i + 1); exit } }'
}

# mac_node MAC: the node whose interface has that MAC, or "none".
mac_node() {
  local n iface
  for n in "${NODES[@]}"; do
    iface="$(node_iface "${n}")"
    if [[ "$(docker exec "${n}" cat "/sys/class/net/${iface}/address")" == "$1" ]]; then
      echo "${n}"
      return
    fi
  done
  echo none
}

observe() {
  local mac
  if ((DRY)); then
    observe_mac
    note "the MAC printed is compared with each node's interface MAC (docker exec <node> cat /sys/class/net/<iface>/address)"
    return 0
  fi
  mac="$(observe_mac)"
  if [[ -z "${mac}" ]]; then
    echo "observe: ${OBSERVER} has no ARP answer for ${VIP}"
    return 0
  fi
  echo "observe: ${OBSERVER} sees ${VIP} at ${mac} = $(mac_node "${mac}")"
}

now() { date +%s.%N; }

wait_holder() {
  poll 60 "exactly one holder of ${VIP}" one_holder
  if ((DRY)); then return 0; fi
  echo "wait-holder: held by $(holders)"
}

fail_probe() {
  local n
  n="$(node_of "$1")"
  say "# the clock starts here: $(now)"
  nx "${NODES[0]}" docker node update --label-rm "${READY_LABEL}" "${n}"
  note "the readiness stand-in leaves ${n}; its check (127.0.0.1:${READY_PORT}) fails after about two runs"
}

kill_holder() {
  local h id
  if ((DRY)); then
    say "# find the node that lists ${VIP}, then:"
    say "docker exec <holder> docker kill -s KILL <its keepalived container>"
    return 0
  fi
  h="$(holders)"
  [[ -n "${h}" && "${h}" != *" "* ]] || die "no single holder to kill (holders: '${h}')"
  id="$(docker exec "${h}" docker ps -q --filter "label=com.docker.swarm.service.name=${SVC}" | head -n 1)"
  [[ -n "${id}" ]] || die "no running keepalived container on ${h}"
  echo "kill-holder: ${h} at $(now)"
  run docker exec "${h}" docker kill -s KILL "${id}"
}

# wait_move NODE [BOUND]: polls the observer until it sees a MAC that is another node's than NODE, and prints the seconds from the start.
wait_move() {
  local from bound="${2:-60}" t0 mac who
  from="$(node_of "$1")"
  if ((DRY)); then
    note "poll the observer for up to ${bound}s until the MAC is not ${from}'s:"
    observe_mac
    return 0
  fi
  t0="$(now)"
  while :; do
    mac="$(observe_mac)"
    who=none
    if [[ -n "${mac}" ]]; then who="$(mac_node "${mac}")"; fi
    if [[ "${who}" != none && "${who}" != "${from}" ]]; then
      awk -v a="$(now)" -v b="${t0}" -v n="${who}" -v m="${mac}" -v f="${from}" 'BEGIN { printf "wait-move: from=%s to=%s mac=%s seconds=%.1f (from the start of this wait)\n", f, n, m, a - b }'
      return 0
    fi
    awk -v a="$(now)" -v b="${t0}" -v s="${bound}" 'BEGIN { exit (a - b > s) ? 0 : 1 }' && die "the observer still sees ${who} after ${bound}s"
  done
}

restore() {
  local n
  for n in "${NODES[@]}"; do
    nx "${NODES[0]}" docker node update --label-add "${READY_LABEL}=yes" "${n}"
  done
}

down() {
  local n
  for n in "${NODES[@]}"; do
    if ! ((DRY)); then
      docker container inspect "${n}" >/dev/null 2>&1 || continue
      if [[ "$(docker container inspect -f '{{index .Config.Labels "tierplan.l2-rig"}}' "${n}")" != "true" ]]; then
        echo "vip-l2-rig: container ${n} does not carry the ${RIG_LABEL} label, so it is not ours; leaving it" >&2
        continue
      fi
    fi
    # -v removes the anonymous volume dind keeps /var/lib/docker in.
    run docker rm -f -v "${n}"
  done
  if ! ((DRY)); then
    docker network inspect "${NET}" >/dev/null 2>&1 || return 0
    if [[ "$(docker network inspect -f '{{index .Labels "tierplan.l2-rig"}}' "${NET}")" != "true" ]]; then
      echo "vip-l2-rig: network ${NET} does not carry the ${RIG_LABEL} label, so it is not ours; leaving it" >&2
      return 0
    fi
  fi
  run docker network rm "${NET}"
}

plan() {
  DRY=1
  configure
  note "PLAN: nothing below runs. Inputs: address ${VIP}/${VIP_BITS}, range ${RANGE}, gateway ${GATEWAY}, parent ${PARENT}, observer ${OBSERVER}"
  note "-- up"
  up
  note "-- holder"
  holder
  note "-- observe"
  observe
  note "-- fail-probe tp-l2-n1 (the holder, whichever it is)"
  fail_probe n1
  wait_move n1
  note "-- restore"
  restore
  note "-- kill-holder, then the move and nopreempt are read with wait-move and holder"
  kill_holder
  wait_move n1
  holder
  note "-- down"
  down
}

usage() {
  echo "usage: $0 plan|check-free|up|holder|observe|wait-holder|fail-probe N|kill-holder|wait-move N|restore|down" >&2
  exit 2
}

cmd="${1:-}"
[[ -n "${cmd}" ]] || usage
shift || true
case "${cmd}" in
  plan)
    plan
    ;;
  down)
    # down needs no inputs: it removes what the fixed names and the label say the rig made.
    down
    ;;
  check-free | up | holder | observe | wait-holder | fail-probe | kill-holder | wait-move | restore)
    configure
    case "${cmd}" in
      check-free) check_free ;;
      up) up ;;
      holder) holder ;;
      observe) observe ;;
      wait-holder) wait_holder ;;
      fail-probe)
        [[ $# -ge 1 ]] || usage
        fail_probe "$1"
        ;;
      kill-holder) kill_holder ;;
      wait-move)
        [[ $# -ge 1 ]] || usage
        wait_move "$@"
        ;;
      restore) restore ;;
    esac
    ;;
  *) usage ;;
esac
