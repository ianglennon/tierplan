#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Runs the built TierPlan image (`make e2e-image` builds tierplan:e2e) behind each of
# Caddy, nginx and Traefik, with the configuration files in docs/proxy, at the root
# of a host and at the subpath /tierplan, and checks what a proxy gets wrong when it
# is set up badly (D-52, PLAT-04):
#
#   - the index carries the tierplan-base meta with the base path
#   - /api/version answers 200
#   - a WebSocket upgrade to /agent/connect with the subprotocol tierplan.agent.v1
#     is answered 101 Switching Protocols
#   - the signed-in event stream sends its first frame at once (no buffering) and
#     the same stream without a session answers 401 promptly
#   - TierPlan shows no proxy warning for a correct setup
#   - shell_ws_reaches_tierplan: the upgrade request of a browser shell
#     (/api/env/<id>/shell-sessions/<id>/ws, with the session cookie and the public
#     Origin) comes back as TierPlan's own JSON problem (environment_not_found, for an
#     environment that does not exist), not a 400, 426 or 502 from the proxy
#   - log_stream_prompt: a container log stream request
#     (/api/env/<id>/containers/<id>/logs/stream) is answered with TierPlan's own JSON
#     404 within 3 seconds, so the proxy does not hold the response back
#
# The two checks above use an environment that does not exist, so no container and no
# Docker socket is needed. They prove the proxy carries the request and its headers to
# TierPlan and returns the answer at once. They do not complete a shell upgrade (101) or
# stream real log lines; the end-to-end suite does that behind Caddy at /tierplan.
#
# It then breaks nginx on purpose, three ways, and checks that TierPlan notices:
# without the two Upgrade header lines the agent endpoint answers 426 and the
# websocket_failing warning appears; without the Host line the url_mismatch
# warning appears; with TierPlan not told to trust the proxy the
# untrusted_forwarded warning appears. A fourth deliberate check, no_agent_switch,
# leaves TierPlan's development switch off and expects the plain agent upgrade to be
# refused 403 insecure_transport through the proxy.
#
# The proxies listen on plain HTTP, so TierPlan's public URL is http://. The Caddy
# file reads TIERPLAN_SCHEME, which this script sets to "http://" so that Caddy does
# not look for a certificate. Requests go to the proxy's published port with the
# Host header of the site, so no DNS entry is needed.
#
# That makes the agent address plain ws:// for a host that is not loopback
# (tierplan.example.com), which TierPlan accepts only with
# TIERPLAN_AGENT_ALLOW_INSECURE=true, a development-only switch. This script sets it
# on TierPlan so that it can test that each proxy passes the upgrade, and only the
# no_agent_switch variant leaves it off to check the refusal through a real proxy.
# The throwaway containers run on a private Docker network with ports published on
# 127.0.0.1 only and hold no secrets.
#
# Environment: IMAGE (default tierplan:e2e), CADDY_IMAGE (caddy:2), NGINX_IMAGE
# (nginx:stable-alpine), TRAEFIK_IMAGE (traefik:v3). Needs Docker, curl and jq.
# Removes every container and the network it creates, by name, and labels them with
# tierplan.ci.owner=$TIERPLAN_CI_OWNER (local-<pid> when unset) so that `make ci-clean`
# in CI removes what a cancelled run left (D-23). The proxy's host port is assigned by
# Docker and read back. Prints one PASS or FAIL line per check and exits 1 if any
# check failed.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-tierplan:e2e}"
CADDY_IMAGE="${CADDY_IMAGE:-caddy:2}"
NGINX_IMAGE="${NGINX_IMAGE:-nginx:stable-alpine}"
TRAEFIK_IMAGE="${TRAEFIK_IMAGE:-traefik:v3}"

for tool in docker curl jq; do
  command -v "${tool}" >/dev/null || { echo "FAIL: ${tool} is required" >&2; exit 1; }
done
docker image inspect "${IMAGE}" >/dev/null 2>&1 || { echo "FAIL: the image ${IMAGE} is not built; run make e2e-image" >&2; exit 1; }

RUN_ID="$$"
OWNER_LABEL="tierplan.ci.owner=${TIERPLAN_CI_OWNER:-local-$$}"
NET="tierplan-proxy-smoke-${RUN_ID}"
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

docker network create --label "${OWNER_LABEL}" "${NET}" >/dev/null
SUBNET="$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' "${NET}")"

# start_container NAME ARGS... runs a detached container on the smoke network and
# remembers it for cleanup.
start_container() {
  local name="$1"
  shift
  CONTAINERS+=("${name}")
  docker run -d --name "${name}" --network "${NET}" --label "${OWNER_LABEL}" "$@" >/dev/null
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
LABEL="" HOST="" BASE="" PUBLIC="" PORT="" SESSION="" PROXY_NAME="" TIERPLAN_NAME=""

# curl_proxy PATH ARGS... talks to the proxy's published port as HOST.
curl_proxy() {
  local path="$1"
  shift
  curl -sS -H "Host: ${HOST}" "$@" "http://127.0.0.1:${PORT}${BASE}${path}"
}

# await_index waits until the proxy serves an index page that carries the tierplan-base
# meta for ${BASE}, or 60 s pass (a deadline, not a count of tries). It sets index_last
# (declared by the caller) to the status and first line of the last response it saw.
await_index() {
  local deadline=$((SECONDS + 60)) body status
  while :; do
    body="$(curl_proxy / --max-time 10 -w $'\n%{http_code}' 2>&1 || true)"
    status="${body##*$'\n'}"
    body="${body%$'\n'*}"
    index_last="${status} $(printf '%s' "${body}" | head -n 1 | cut -c1-200)"
    if grep -qF "<meta name=\"tierplan-base\" content=\"${BASE}\">" <<<"${body}"; then
      return 0
    fi
    [ "${SECONDS}" -ge "${deadline}" ] && return 1
    sleep 1
  done
}

wait_tierplan() {
  local state="" _
  for _ in $(seq 1 60); do
    state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${TIERPLAN_NAME}")"
    [ "${state}" = "healthy" ] && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "${TIERPLAN_NAME}")" = "true" ] || break
    sleep 1
  done
  docker logs "${TIERPLAN_NAME}" 2>&1 | tail -n 20 >&2 || true
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
      start_container "${PROXY_NAME}" -p 127.0.0.1::80 -e TIERPLAN_SCHEME=http:// \
        -v "${conf}:/etc/caddy/Caddyfile:ro" "${CADDY_IMAGE}" ;;
    nginx)
      start_container "${PROXY_NAME}" -p 127.0.0.1::80 \
        -v "${conf}:/etc/nginx/nginx.conf:ro" "${NGINX_IMAGE}" ;;
    traefik)
      start_container "${PROXY_NAME}" -p 127.0.0.1::80 \
        -v "${conf}:/etc/traefik/tierplan.yml:ro" "${TRAEFIK_IMAGE}" \
        --entrypoints.web.address=:80 --providers.file.filename=/etc/traefik/tierplan.yml ;;
  esac
  PORT="$(docker port "${PROXY_NAME}" 80/tcp | head -n 1 | sed 's/.*://')"
}

# upgrade_request sends the agent's WebSocket upgrade and leaves the response head
# in ${WORK}/upgrade.out. The connection stays open after a 101, so curl is cut
# off by its time limit; that is not a failure.
upgrade_request() {
  curl -sS -i --http1.1 --max-time 3 -o "${WORK}/upgrade.out" -H "Host: ${HOST}" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H 'Sec-WebSocket-Protocol: tierplan.agent.v1' \
    "http://127.0.0.1:${PORT}${BASE}/agent/connect" 2>/dev/null || true
  head -n 1 "${WORK}/upgrade.out" | tr -d '\r'
}

# problem_type prints the type of the JSON problem in the file given, or nothing.
problem_type() {
  jq -r '.type // empty' "$1" 2>/dev/null || true
}

# A signed-in request to an environment that does not exist is answered by TierPlan's
# policy before any handler runs, so the answer is the same on every proxy and needs
# no container.
MISSING_ENV="01980000-0000-7000-8000-0000000000ff"

# shell_ws_request sends the upgrade request of a browser shell and leaves the head
# and body of the answer in ${WORK}/shellws.out. The status line is printed.
shell_ws_request() {
  curl -sS -i --http1.1 --max-time 5 -o "${WORK}/shellws.out" -H "Host: ${HOST}" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H "Origin: http://${HOST}" \
    -H "Cookie: tierplan_session=${SESSION}" \
    "http://127.0.0.1:${PORT}${BASE}/api/env/${MISSING_ENV}/shell-sessions/x/ws" 2>/dev/null || true
  head -n 1 "${WORK}/shellws.out" | tr -d '\r'
}

# warnings prints the ids of the proxy warnings the signed-in admin sees, sorted.
warnings() {
  curl_proxy /api/warnings -H "Cookie: tierplan_session=${SESSION}" --max-time 5 \
    | jq -r '[.items[].id | select(startswith("proxy_"))] | sort | join(" ")'
}

# sign_in completes setup through the proxy and keeps the session cookie.
sign_in() {
  local token code
  token="$(docker exec "${TIERPLAN_NAME}" /usr/local/bin/tierplan setup-token)" || return 1
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
    root) HOST="tierplan.example.com" BASE="" ;;
    subpath) HOST="example.com" BASE="/tierplan" ;;
  esac
  PUBLIC="http://${HOST}${BASE}"
  LABEL="${kind} ${site}"
  [ "${variant}" = ok ] || LABEL="${LABEL} (${variant})"
  TIERPLAN_NAME="tierplan-psmoke-${RUN_ID}-cm"
  PROXY_NAME="tierplan-psmoke-${RUN_ID}-px"
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
  # case, so every variant but no_agent_switch sets TierPlan's development switch.
  agent_switch=1
  [ "${variant}" = no_agent_switch ] && agent_switch=""
  start_container "${TIERPLAN_NAME}" --network-alias tierplan --tmpfs /data \
    -e "TIERPLAN_PUBLIC_URL=${PUBLIC}" ${trust:+-e "TIERPLAN_TRUSTED_PROXIES=${trust}"} \
    ${agent_switch:+-e "TIERPLAN_AGENT_ALLOW_INSECURE=true"} \
    --health-interval 2s --health-start-period 0s --health-retries 10 "${IMAGE}"
  if ! wait_tierplan; then
    fail "${LABEL}: TierPlan did not become healthy"
    stop_container "${TIERPLAN_NAME}"
    return
  fi
  start_proxy "${kind}" "${conf}"
  if ! wait_proxy; then
    fail "${LABEL}: the proxy never passed /healthz through to TierPlan"
    stop_container "${PROXY_NAME}"
    stop_container "${TIERPLAN_NAME}"
    return
  fi

  local ok_signed=1
  sign_in || { fail "${LABEL}: setup through the proxy failed"; ok_signed=0; }

  # The index, with the base path the public URL decides. The proxy and the server are
  # external processes: the wait is for the proxy to serve the first full page after
  # setup, bounded by a deadline. A wrong answer is not tried again; the failure says
  # what the last response was.
  local index_last=""
  if await_index; then
    pass "${LABEL}: the index carries the tierplan-base meta \"${BASE}\""
  else
    fail "${LABEL}: the index has no tierplan-base meta \"${BASE}\" after 60s; last response: ${index_last}"
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
        if grep -qai '^sec-websocket-protocol: tierplan.agent.v1' "${WORK}/upgrade.out"; then
          pass "${LABEL}: ${BASE}/agent/connect upgrades to a WebSocket with tierplan.agent.v1 (${status})"
        else
          fail "${LABEL}: the upgrade was accepted without the tierplan.agent.v1 subprotocol"
        fi
      elif [ "${want}" = 403 ]; then
        if grep -qa 'insecure_transport' "${WORK}/upgrade.out"; then
          pass "${LABEL}: a plain agent upgrade without TIERPLAN_AGENT_ALLOW_INSECURE is refused with ${status} insecure_transport"
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
    curl_proxy /api/environments/stream -H "Cookie: tierplan_session=${SESSION}" -N --max-time 4 >"${WORK}/sse.out" 2>/dev/null || true
    if grep -q '^event: snapshot' "${WORK}/sse.out"; then
      pass "${LABEL}: the event stream's first frame arrives within 4 s, unbuffered"
    else
      fail "${LABEL}: the event stream sent nothing within 4 s (a proxy is buffering it)"
    fi

    if [ "${variant}" = ok ]; then
      # shell_ws_reaches_tierplan: the shell's WebSocket upgrade request gets to TierPlan.
      local shell_status shell_type
      shell_status="$(shell_ws_request)"
      shell_type="$(sed '1,/^\r\?$/d' "${WORK}/shellws.out" | jq -r '.type // empty' 2>/dev/null || true)"
      case "${shell_status}" in
        *" 404 "*)
          if [ "${shell_type}" = environment_not_found ]; then
            pass "${LABEL}: shell_ws_reaches_tierplan: the shell upgrade request reaches TierPlan (${shell_status}, ${shell_type})"
          else
            fail "${LABEL}: shell_ws_reaches_tierplan: ${shell_status} but the body is not TierPlan's environment_not_found problem ('${shell_type}')"
          fi ;;
        *) fail "${LABEL}: shell_ws_reaches_tierplan: the shell upgrade request answered '${shell_status}', want TierPlan's 404 environment_not_found" ;;
      esac

      # log_stream_prompt: a log stream request is answered at once, not held back.
      local log_timing log_type
      log_timing="$(curl_proxy "/api/env/${MISSING_ENV}/containers/x/logs/stream?sid=abcdefghijklmnopqrstuv" \
        -H "Cookie: tierplan_session=${SESSION}" -H 'Accept: text/event-stream' -N \
        -o "${WORK}/logstream.out" -w '%{http_code} %{time_total}' --max-time 5 || true)"
      log_type="$(problem_type "${WORK}/logstream.out")"
      if [ "${log_timing%% *}" = 404 ] && [ "${log_type}" = environment_not_found ] && awk -v t="${log_timing##* }" 'BEGIN { exit !(t < 3) }'; then
        pass "${LABEL}: log_stream_prompt: the log stream request is answered by TierPlan at once (${log_timing##* } s)"
      else
        fail "${LABEL}: log_stream_prompt: the log stream request answered '${log_timing}' type '${log_type}', want 404 environment_not_found in under 3 s"
      fi
    fi

    # What TierPlan itself says about the proxy.
    local seen
    seen="$(warnings || true)"
    case "${variant}" in
      ok | no_agent_switch)
        if [ -z "${seen}" ]; then pass "${LABEL}: TierPlan shows no proxy warning"; else fail "${LABEL}: TierPlan shows proxy warnings: ${seen}"; fi ;;
      no_upgrade_headers)
        if [ "${seen}" = proxy_websocket_failing ]; then pass "${LABEL}: TierPlan shows proxy_websocket_failing"; else fail "${LABEL}: warnings are '${seen}', want proxy_websocket_failing"; fi ;;
      no_host_header)
        if [ "${seen}" = proxy_url_mismatch ]; then pass "${LABEL}: TierPlan shows proxy_url_mismatch"; else fail "${LABEL}: warnings are '${seen}', want proxy_url_mismatch"; fi ;;
      untrusted_proxy)
        if [ "${seen}" = proxy_untrusted_forwarded ]; then pass "${LABEL}: TierPlan shows proxy_untrusted_forwarded"; else fail "${LABEL}: warnings are '${seen}', want proxy_untrusted_forwarded"; fi ;;
    esac
  fi

  stop_container "${PROXY_NAME}"
  stop_container "${TIERPLAN_NAME}"
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
