#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Runs the built Bayplan image (`make e2e-image` builds bayplan:e2e) behind each of
# Caddy, nginx and Traefik, with the configuration files in docs/proxy, at the root
# of a host and at the subpath /bayplan, and checks what a proxy gets wrong when it
# is set up badly (D-52, PLAT-04):
#
#   - the index carries the bayplan-base meta with the base path
#   - /api/version answers 200
#   - a WebSocket upgrade to /agent/connect with the subprotocol bayplan.agent.v1
#     is answered 101 Switching Protocols
#   - the signed-in event stream sends its first frame at once (no buffering) and
#     the same stream without a session answers 401 promptly
#   - Bayplan shows no proxy warning for a correct setup
#
# It then breaks nginx on purpose, three ways, and checks that Bayplan notices:
# without the two Upgrade header lines the agent endpoint answers 426 and the
# websocket_failing warning appears; without the Host line the url_mismatch
# warning appears; with Bayplan not told to trust the proxy the
# untrusted_forwarded warning appears. A fourth deliberate check, no_agent_switch,
# leaves Bayplan's development switch off and expects the plain agent upgrade to be
# refused 403 insecure_transport through the proxy.
#
# The proxies listen on plain HTTP, so Bayplan's public URL is http://. The Caddy
# file reads BAYPLAN_SCHEME, which this script sets to "http://" so that Caddy does
# not look for a certificate. Requests go to the proxy's published port with the
# Host header of the site, so no DNS entry is needed.
#
# That makes the agent address plain ws:// for a host that is not loopback
# (bayplan.example.com), which Bayplan accepts only with
# BAYPLAN_AGENT_ALLOW_INSECURE=true, a development-only switch. This script sets it
# on Bayplan so that it can test that each proxy passes the upgrade, and only the
# no_agent_switch variant leaves it off to check the refusal through a real proxy.
# The throwaway containers run on a private Docker network with ports published on
# 127.0.0.1 only and hold no secrets.
#
# Environment: IMAGE (default bayplan:e2e), CADDY_IMAGE (caddy:2), NGINX_IMAGE
# (nginx:stable-alpine), TRAEFIK_IMAGE (traefik:v3). Needs Docker, curl and jq.
# Removes every container and the network it creates, by name. Prints one PASS or
# FAIL line per check and exits 1 if any check failed.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-bayplan:e2e}"
CADDY_IMAGE="${CADDY_IMAGE:-caddy:2}"
NGINX_IMAGE="${NGINX_IMAGE:-nginx:stable-alpine}"
TRAEFIK_IMAGE="${TRAEFIK_IMAGE:-traefik:v3}"

for tool in docker curl jq; do
  command -v "${tool}" >/dev/null || { echo "FAIL: ${tool} is required" >&2; exit 1; }
done
docker image inspect "${IMAGE}" >/dev/null 2>&1 || { echo "FAIL: the image ${IMAGE} is not built; run make e2e-image" >&2; exit 1; }

RUN_ID="$$"
NET="bayplan-proxy-smoke-${RUN_ID}"
WORK="$(mktemp -d)"
JSON_H='Content-Type: application/json'
PW='correct horse battery'
FAILS=0
CONTAINERS=()

cleanup() {
  local c
  for c in ${CONTAINERS[@]+"${CONTAINERS[@]}"}; do
    docker rm -f "${c}" >/dev/null 2>&1 || true
  done
  docker network rm "${NET}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }

docker network create "${NET}" >/dev/null
SUBNET="$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' "${NET}")"

# start_container NAME ARGS... runs a detached container on the smoke network and
# remembers it for cleanup.
start_container() {
  local name="$1"
  shift
  CONTAINERS+=("${name}")
  docker run -d --name "${name}" --network "${NET}" "$@" >/dev/null
}

stop_container() {
  local name="$1" keep=() c
  docker rm -f "${name}" >/dev/null 2>&1 || true
  for c in ${CONTAINERS[@]+"${CONTAINERS[@]}"}; do
    [ "${c}" = "${name}" ] || keep+=("${c}")
  done
  CONTAINERS=(${keep[@]+"${keep[@]}"})
}

# Per-scenario state, set by run_scenario.
LABEL="" HOST="" BASE="" PUBLIC="" PORT="" SESSION="" PROXY_NAME="" BAYPLAN_NAME=""

# curl_proxy PATH ARGS... talks to the proxy's published port as HOST.
curl_proxy() {
  local path="$1"
  shift
  curl -sS -H "Host: ${HOST}" "$@" "http://127.0.0.1:${PORT}${BASE}${path}"
}

wait_bayplan() {
  local state="" _
  for _ in $(seq 1 60); do
    state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${BAYPLAN_NAME}")"
    [ "${state}" = "healthy" ] && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "${BAYPLAN_NAME}")" = "true" ] || break
    sleep 1
  done
  docker logs "${BAYPLAN_NAME}" 2>&1 | tail -n 20 >&2 || true
  return 1
}

wait_proxy() {
  local code="" _
  for _ in $(seq 1 60); do
    code="$(curl_proxy /healthz -o /dev/null -w '%{http_code}' --max-time 3 2>/dev/null || true)"
    [ "${code}" = "200" ] && return 0
    sleep 0.5
  done
  docker logs "${PROXY_NAME}" 2>&1 | tail -n 20 >&2 || true
  return 1
}

# start_proxy KIND CONFIG starts the proxy of KIND with CONFIG mounted.
start_proxy() {
  local kind="$1" conf="$2"
  case "${kind}" in
    caddy)
      start_container "${PROXY_NAME}" -p 127.0.0.1::80 -e BAYPLAN_SCHEME=http:// \
        -v "${conf}:/etc/caddy/Caddyfile:ro" "${CADDY_IMAGE}" ;;
    nginx)
      start_container "${PROXY_NAME}" -p 127.0.0.1::80 \
        -v "${conf}:/etc/nginx/nginx.conf:ro" "${NGINX_IMAGE}" ;;
    traefik)
      start_container "${PROXY_NAME}" -p 127.0.0.1::80 \
        -v "${conf}:/etc/traefik/bayplan.yml:ro" "${TRAEFIK_IMAGE}" \
        --entrypoints.web.address=:80 --providers.file.filename=/etc/traefik/bayplan.yml ;;
  esac
  PORT="$(docker port "${PROXY_NAME}" 80/tcp | head -n 1 | sed 's/.*://')"
}

# upgrade_request sends the agent's WebSocket upgrade and leaves the response head
# in ${WORK}/upgrade.out. The connection stays open after a 101, so curl is cut
# off by its time limit; that is not a failure.
upgrade_request() {
  curl -sS -i --http1.1 --max-time 3 -o "${WORK}/upgrade.out" -H "Host: ${HOST}" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H 'Sec-WebSocket-Protocol: bayplan.agent.v1' \
    "http://127.0.0.1:${PORT}${BASE}/agent/connect" 2>/dev/null || true
  head -n 1 "${WORK}/upgrade.out" | tr -d '\r'
}

# warnings prints the ids of the proxy warnings the signed-in admin sees, sorted.
warnings() {
  curl_proxy /api/warnings -H "Cookie: bayplan_session=${SESSION}" --max-time 5 \
    | jq -r '[.items[].id | select(startswith("proxy_"))] | sort | join(" ")'
}

# sign_in completes setup through the proxy and keeps the session cookie.
sign_in() {
  local token code
  token="$(docker exec "${BAYPLAN_NAME}" /usr/local/bin/bayplan setup-token)" || return 1
  code="$(curl_proxy /api/setup/complete -o "${WORK}/complete.json" -D "${WORK}/complete.hdr" -w '%{http_code}' \
    -H "${JSON_H}" --max-time 10 \
    -d "{\"token\":\"${token}\",\"username\":\"admin\",\"password\":\"${PW}\",\"instanceName\":\"Proxy smoke\",\"publicUrl\":\"${PUBLIC}\"}")"
  [ "${code}" = "200" ] || { cat "${WORK}/complete.json" >&2; return 1; }
  SESSION="$(grep -i '^set-cookie:' "${WORK}/complete.hdr" | tr -d '\r' | sed 's/^[^=]*=//; s/;.*//')"
  [ -n "${SESSION}" ]
}

# run_scenario KIND SITE VARIANT: KIND is caddy, nginx or traefik, SITE is root or
# subpath, VARIANT is ok for the documented file or the name of a deliberate break.
run_scenario() {
  local kind="$1" site="$2" variant="${3:-ok}" conf trust agent_switch
  case "${site}" in
    root) HOST="bayplan.example.com" BASE="" ;;
    subpath) HOST="example.com" BASE="/bayplan" ;;
  esac
  PUBLIC="http://${HOST}${BASE}"
  LABEL="${kind} ${site}"
  [ "${variant}" = ok ] || LABEL="${LABEL} (${variant})"
  BAYPLAN_NAME="bayplan-psmoke-${RUN_ID}-bp"
  PROXY_NAME="bayplan-psmoke-${RUN_ID}-px"
  SESSION=""

  case "${kind}" in
    caddy) conf="docs/proxy/Caddyfile" ;;
    nginx) conf="docs/proxy/nginx.conf" ;;
    traefik) conf="docs/proxy/traefik.yml" ;;
  esac
  case "${variant}" in
    no_upgrade_headers)
      grep -vE 'proxy_set_header (Upgrade|Connection) ' "${conf}" >"${WORK}/${variant}.conf"
      conf="${WORK}/${variant}.conf" ;;
    no_host_header)
      grep -vE 'proxy_set_header Host ' "${conf}" >"${WORK}/${variant}.conf"
      conf="${WORK}/${variant}.conf" ;;
  esac
  conf="$(cd "$(dirname "${conf}")" && pwd)/$(basename "${conf}")"
  trust="${SUBNET}"
  [ "${variant}" = untrusted_proxy ] && trust=""

  # The proxies are plain HTTP for a host that is not loopback, the development-only
  # case, so every variant but no_agent_switch sets Bayplan's development switch.
  agent_switch=1
  [ "${variant}" = no_agent_switch ] && agent_switch=""
  start_container "${BAYPLAN_NAME}" --network-alias bayplan --tmpfs /data \
    -e "BAYPLAN_PUBLIC_URL=${PUBLIC}" ${trust:+-e "BAYPLAN_TRUSTED_PROXIES=${trust}"} \
    ${agent_switch:+-e "BAYPLAN_AGENT_ALLOW_INSECURE=true"} \
    --health-interval 2s --health-start-period 0s --health-retries 10 "${IMAGE}"
  if ! wait_bayplan; then
    fail "${LABEL}: Bayplan did not become healthy"
    stop_container "${BAYPLAN_NAME}"
    return
  fi
  start_proxy "${kind}" "${conf}"
  if ! wait_proxy; then
    fail "${LABEL}: the proxy never passed /healthz through to Bayplan"
    stop_container "${PROXY_NAME}"
    stop_container "${BAYPLAN_NAME}"
    return
  fi

  local ok_signed=1
  sign_in || { fail "${LABEL}: setup through the proxy failed"; ok_signed=0; }

  # The index, with the base path the public URL decides.
  if curl_proxy / --max-time 5 | grep -qF "<meta name=\"bayplan-base\" content=\"${BASE}\">"; then
    pass "${LABEL}: the index carries the bayplan-base meta \"${BASE}\""
  else
    fail "${LABEL}: the index has no bayplan-base meta \"${BASE}\""
  fi
  local code
  code="$(curl_proxy /api/version -o /dev/null -w '%{http_code}' --max-time 5 || true)"
  if [ "${code}" = 200 ]; then pass "${LABEL}: ${BASE}/api/version is 200"; else fail "${LABEL}: ${BASE}/api/version is ${code}, want 200"; fi

  # The WebSocket upgrade of the agent endpoint.
  local status want="101"
  [ "${variant}" = no_upgrade_headers ] && want="426"
  [ "${variant}" = no_agent_switch ] && want="403"
  status="$(upgrade_request)"
  case "${status}" in
    *" ${want} "*)
      if [ "${want}" = 101 ]; then
        if grep -qai '^sec-websocket-protocol: bayplan.agent.v1' "${WORK}/upgrade.out"; then
          pass "${LABEL}: ${BASE}/agent/connect upgrades to a WebSocket with bayplan.agent.v1 (${status})"
        else
          fail "${LABEL}: the upgrade was accepted without the bayplan.agent.v1 subprotocol"
        fi
      elif [ "${want}" = 403 ]; then
        if grep -qa 'insecure_transport' "${WORK}/upgrade.out"; then
          pass "${LABEL}: a plain agent upgrade without BAYPLAN_AGENT_ALLOW_INSECURE is refused with ${status} insecure_transport"
        else
          fail "${LABEL}: ${BASE}/agent/connect answered ${status} without insecure_transport in the body"
        fi
      else
        pass "${LABEL}: a proxy that drops the Upgrade headers gets ${status} from ${BASE}/agent/connect"
      fi ;;
    *) fail "${LABEL}: ${BASE}/agent/connect answered '${status}', want ${want}" ;;
  esac

  # Server-Sent Events without a session, then with one.
  local timing
  timing="$(curl_proxy /api/environments/stream -o /dev/null -w '%{http_code} %{time_total}' --max-time 5 || true)"
  if [ "${timing%% *}" = 401 ] && awk -v t="${timing##* }" 'BEGIN { exit !(t < 3) }'; then
    pass "${LABEL}: the event stream without a session answers 401 at once (${timing##* } s)"
  else
    fail "${LABEL}: the event stream without a session answered '${timing}', want 401 in under 3 s"
  fi
  if [ "${ok_signed}" = 1 ]; then
    curl_proxy /api/environments/stream -H "Cookie: bayplan_session=${SESSION}" -N --max-time 4 >"${WORK}/sse.out" 2>/dev/null || true
    if grep -q '^event: snapshot' "${WORK}/sse.out"; then
      pass "${LABEL}: the event stream's first frame arrives within 4 s, unbuffered"
    else
      fail "${LABEL}: the event stream sent nothing within 4 s (a proxy is buffering it)"
    fi

    # What Bayplan itself says about the proxy.
    local seen
    seen="$(warnings || true)"
    case "${variant}" in
      ok | no_agent_switch)
        if [ -z "${seen}" ]; then pass "${LABEL}: Bayplan shows no proxy warning"; else fail "${LABEL}: Bayplan shows proxy warnings: ${seen}"; fi ;;
      no_upgrade_headers)
        if [ "${seen}" = proxy_websocket_failing ]; then pass "${LABEL}: Bayplan shows proxy_websocket_failing"; else fail "${LABEL}: warnings are '${seen}', want proxy_websocket_failing"; fi ;;
      no_host_header)
        if [ "${seen}" = proxy_url_mismatch ]; then pass "${LABEL}: Bayplan shows proxy_url_mismatch"; else fail "${LABEL}: warnings are '${seen}', want proxy_url_mismatch"; fi ;;
      untrusted_proxy)
        if [ "${seen}" = proxy_untrusted_forwarded ]; then pass "${LABEL}: Bayplan shows proxy_untrusted_forwarded"; else fail "${LABEL}: warnings are '${seen}', want proxy_untrusted_forwarded"; fi ;;
    esac
  fi

  stop_container "${PROXY_NAME}"
  stop_container "${BAYPLAN_NAME}"
}

# The guide embeds each file. A line of a file that is missing from the guide means
# the guide shows something other than what is tested here.
for conf in Caddyfile nginx.conf traefik.yml; do
  stale=""
  while IFS= read -r line; do
    [ -n "${line}" ] || continue
    grep -qxF -- "${line}" docs/reverse-proxy.md || stale="${line}"
  done < <(grep -vF 'SPDX-' "docs/proxy/${conf}")
  if [ -z "${stale}" ]; then
    pass "docs/reverse-proxy.md shows docs/proxy/${conf} as it is tested"
  else
    fail "docs/reverse-proxy.md does not show this line of docs/proxy/${conf}: ${stale}"
  fi
done

for kind in caddy nginx traefik; do
  for site in root subpath; do
    run_scenario "${kind}" "${site}" ok
  done
done
run_scenario nginx root no_upgrade_headers
run_scenario nginx root no_host_header
run_scenario nginx root untrusted_proxy
run_scenario nginx root no_agent_switch

if [ "${FAILS}" -ne 0 ]; then
  echo "FAIL: ${FAILS} proxy smoke check(s) failed"
  exit 1
fi
echo "PROXY SMOKE OK"
