#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Pebble, the Let's Encrypt test CA, for trying the Settings > HTTPS tab by hand on
# the development host. The automated suite starts its own Pebble (internal/acme/acmetest);
# this one is for a person with a browser.
#
#   scripts/dev-pebble.sh up      start Pebble and print the two variables TierPlan needs
#   scripts/dev-pebble.sh down    remove it
#
# `up` writes .devdata/pebble/pebble-config.json and .devdata/pebble/ca.pem (Pebble's
# test CA), starts the container tierplan-dev-pebble, and prints
#   TIERPLAN_ACME_DIRECTORY_URL=https://localhost:14000/dir
#   TIERPLAN_ACME_CA_FILE=<absolute path of ca.pem>
# Export both before `make dev`. The CA reaches TierPlan only through the second
# variable, for its requests to Pebble; nothing is added to the system's trust store.
# Pebble's ports are published on 127.0.0.1 only.
#
# Pebble checks an HTTP-01 challenge by fetching http://<name>:<port>/.well-known/acme-challenge/
# from inside its container, so the container's /etc/hosts maps <name> to the Docker host
# (host-gateway) and <port> is where TierPlan's listener is.
#
# While TierPlan serves a certificate Pebble issued, anything that checks it against the public
# roots refuses it: the dev swarm's agents drop and dial again once Let's Encrypt is stopped.
#
# Environment (all optional):
#   DEV_PEBBLE_HOST        the name the certificate is asked for, which is the host name of the
#                          public URL; default: this machine's fully qualified name
#   DEV_PEBBLE_HTTP_PORT   the port TierPlan listens on (the port of DEV_LISTEN), default 7480
#   DEV_PEBBLE_DNS_SERVER  host:port of the resolver Pebble asks for a DNS-01 challenge's TXT record
#                          (Pebble's -dnsserver flag, the one the CI Pebble uses), for example
#                          1.1.1.1:53. Unset, Pebble resolves through the host's resolver, which on
#                          a LAN with split-horizon DNS may not see the public record.
#
# `down` removes only the container tierplan-dev-pebble, and only when it carries the label
# `up` put on it. It never prunes.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE=ghcr.io/letsencrypt/pebble:2.10.1
NAME=tierplan-dev-pebble
LABEL=tierplan.dev-pebble=true
DIR="$(pwd)/.devdata/pebble"
HTTP_PORT="${DEV_PEBBLE_HTTP_PORT:-7480}"
HOST="${DEV_PEBBLE_HOST:-$(hostname -f)}"
DNS_SERVER="${DEV_PEBBLE_DNS_SERVER:-}"

exists() { docker container inspect "$1" >/dev/null 2>&1; }

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
  echo "dev-pebble: gave up waiting for ${what} after ${deadline}s" >&2
  return 1
}

up() {
  if [[ ! "${HTTP_PORT}" =~ ^[1-9][0-9]{0,4}$ ]] || ((HTTP_PORT > 65535)); then
    echo "dev-pebble: DEV_PEBBLE_HTTP_PORT must be a port number, not '${HTTP_PORT}'" >&2
    exit 2
  fi
  # host:port, where host is a name or an address and the port is a number.
  if [[ -n "${DNS_SERVER}" ]]; then
    local dns_port="${DNS_SERVER##*:}"
    if [[ "${DNS_SERVER}" != *:* || ! "${DNS_SERVER}" =~ ^[A-Za-z0-9._-]+:[1-9][0-9]{0,4}$ ]] || ((dns_port > 65535)); then
      echo "dev-pebble: DEV_PEBBLE_DNS_SERVER must be host:port, not '${DNS_SERVER}'" >&2
      exit 2
    fi
  fi
  if exists "${NAME}"; then
    echo "dev-pebble: container ${NAME} already exists; run 'scripts/dev-pebble.sh down' first" >&2
    exit 1
  fi
  mkdir -p "${DIR}"
  # The certificate and key are the image's own (made for localhost); the validity periods
  # are Pebble's two profiles, and the retry delays are short so a run does not sit waiting.
  cat >"${DIR}/pebble-config.json" <<JSON
{
  "pebble": {
    "listenAddress": "0.0.0.0:14000",
    "managementListenAddress": "0.0.0.0:15000",
    "certificate": "/test/certs/localhost/cert.pem",
    "privateKey": "/test/certs/localhost/key.pem",
    "httpPort": ${HTTP_PORT},
    "tlsPort": 5001,
    "ocspResponderURL": "",
    "externalAccountBindingRequired": false,
    "domainBlocklist": ["blocked-domain.example"],
    "retryAfter": { "authz": 1, "order": 1 },
    "keyAlgorithm": "ecdsa",
    "profiles": {
      "default": { "description": "The default profile", "validityPeriod": 7776000 },
      "shortlived": { "description": "A short-lived profile", "validityPeriod": 518400 }
    }
  }
}
JSON
  # PEBBLE_VA_NOSLEEP drops the random validation delay, PEBBLE_WFE_NONCEREJECT=0 stops it
  # rejecting nonces on purpose and PEBBLE_AUTHZREUSE=0 stops it reusing authorizations, so a
  # run is the same every time.
  local dns_args=()
  if [[ -n "${DNS_SERVER}" ]]; then
    dns_args=(-dnsserver "${DNS_SERVER}")
  fi
  docker run -d --name "${NAME}" --label "${LABEL}" \
    --add-host "${HOST}:host-gateway" \
    -p 127.0.0.1:14000:14000 -p 127.0.0.1:15000:15000 \
    -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 -e PEBBLE_AUTHZREUSE=0 \
    -v "${DIR}/pebble-config.json:/cfg/pebble-config.json:ro" \
    "${IMAGE}" -config /cfg/pebble-config.json "${dns_args[@]}" >/dev/null
  # The image has no shell, but docker cp reads a file out of any container.
  docker cp "${NAME}:/test/certs/pebble.minica.pem" "${DIR}/ca.pem"
  poll 60 "Pebble's directory" curl -sS --fail --cacert "${DIR}/ca.pem" https://localhost:14000/dir

  cat <<EOF

Pebble is up. It fetches http://${HOST}:${HTTP_PORT}/.well-known/acme-challenge/ to check a name.
Export these before starting TierPlan (listening on port ${HTTP_PORT}, public URL host ${HOST}):

export TIERPLAN_ACME_DIRECTORY_URL=https://localhost:14000/dir
export TIERPLAN_ACME_CA_FILE=${DIR}/ca.pem

Take it down with: scripts/dev-pebble.sh down
EOF
}

down() {
  if ! exists "${NAME}"; then
    return 0
  fi
  if [[ "$(docker container inspect -f '{{index .Config.Labels "tierplan.dev-pebble"}}' "${NAME}")" != "true" ]]; then
    echo "dev-pebble: container ${NAME} does not carry the ${LABEL} label, so it is not ours; leaving it" >&2
    return 0
  fi
  docker rm -f "${NAME}" >/dev/null
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *)
    echo "usage: $0 up|down" >&2
    exit 2
    ;;
esac
