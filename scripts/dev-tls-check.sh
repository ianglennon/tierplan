#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Dev loop over TLS (plan 13.1-05, D-08): starts the Go server on a scratch data
# directory and the Vite dev server over https with a freshly made development
# certificate, then proves three things with a client that verifies:
#   (a) the browser side: https://localhost:<vite>/api/version answers 200, through
#       Vite's https listener and its proxy, which verifies the Go server's own
#       certificate (its mirror, tls/server.crt) as the only trust anchor;
#   (b) the proxy checks what it connects to: with a different certificate as the
#       only anchor the same request is refused (a 5xx), not passed through;
#   (c) there is no plain dev UI: http://localhost:<vite>/ gets no answer.
# It starts no container and leaves nothing running. Needs go, node, pnpm
# dependencies installed (web/node_modules) and curl. No certificate check is ever
# turned off here or in web/vite.config.ts.
set -euo pipefail

cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
GO_PID=""
VITE_PID=""
failures=0
passes=0

stop_vite() {
  if [ -n "${VITE_PID}" ]; then
    kill "${VITE_PID}" 2>/dev/null || true
    wait "${VITE_PID}" 2>/dev/null || true
    VITE_PID=""
  fi
}
cleanup() {
  stop_vite
  if [ -n "${GO_PID}" ]; then
    kill "${GO_PID}" 2>/dev/null || true
    wait "${GO_PID}" 2>/dev/null || true
  fi
  rm -rf "${WORK}"
}
trap cleanup EXIT

die() {
  echo "FAIL: $1" >&2
  for f in go.log vite.log; do
    if [ -s "${WORK}/${f}" ]; then
      echo "--- ${f} ---" >&2
      cat "${WORK}/${f}" >&2
    fi
  done
  exit 1
}
pass() {
  echo "PASS: $1"
  passes=$((passes + 1))
}
fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

[ -x web/node_modules/.bin/vite ] || die "web/node_modules/.bin/vite is missing; run pnpm -C web install"

# A free port for Vite. The probe releases it before Vite binds it, which is why Vite
# runs with --strictPort: a collision fails loudly instead of moving.
VITE_PORT="$(node -e 'const s=require("net").createServer();s.listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
[[ "${VITE_PORT}" =~ ^[0-9]+$ ]] || die "could not pick a port for Vite"

go build -o "${WORK}/tierplan" ./cmd/tierplan || die "go build ./cmd/tierplan failed"
go run scripts/dev-cert.go -out "${WORK}/dev-tls" >"${WORK}/dev-cert.log" 2>&1 || {
  cat "${WORK}/dev-cert.log" >&2
  die "scripts/dev-cert.go failed"
}
CA="${WORK}/dev-tls/ca.pem"
[ -s "${CA}" ] || die "dev-cert wrote no ca.pem"

# The Go server: default self-signed mode, a kernel-chosen port read from its own
# "tierplan listening" line.
DATA="${WORK}/data"
(cd "${WORK}" && TIERPLAN_DATA_DIR="${DATA}" TIERPLAN_LISTEN="127.0.0.1:0" \
  TIERPLAN_PUBLIC_URL="https://localhost:${VITE_PORT}" exec "${WORK}/tierplan" serve) >"${WORK}/go.log" 2>&1 &
GO_PID=$!

GO_PORT=""
for _ in $(seq 1 200); do
  line="$(grep -m1 '"msg":"tierplan listening"' "${WORK}/go.log" 2>/dev/null || true)"
  if [ -n "${line}" ]; then
    addr="$(printf '%s' "${line}" | sed 's/.*"addr":"\([^"]*\)".*/\1/')"
    GO_PORT="${addr##*:}"
    break
  fi
  kill -0 "${GO_PID}" 2>/dev/null || die "the Go server exited during startup"
  sleep 0.1
done
[[ "${GO_PORT}" =~ ^[0-9]+$ ]] || die "the Go server did not log 'tierplan listening' within 20 seconds"
MIRROR="${DATA}/tls/server.crt"
[ -s "${MIRROR}" ] || die "the Go server wrote no ${MIRROR}"

# start_vite <upstream trust file>: Vite over https with the dev certificate, proxying to
# the Go server and trusting only the given file for it.
start_vite() {
  (cd web && TIERPLAN_DEV_TLS_DIR="${WORK}/dev-tls" TIERPLAN_DEV_SERVER_CERT="$1" \
    TIERPLAN_DEV_API_TARGET="https://127.0.0.1:${GO_PORT}" \
    exec node_modules/.bin/vite --port "${VITE_PORT}" --strictPort) >"${WORK}/vite.log" 2>&1 &
  VITE_PID=$!
  local i
  for ((i = 0; i < 300; i++)); do
    if curl -s -o /dev/null --cacert "${CA}" --max-time 5 "https://localhost:${VITE_PORT}/"; then
      return 0
    fi
    kill -0 "${VITE_PID}" 2>/dev/null || die "Vite exited during startup"
    sleep 0.1
  done
  die "Vite did not answer over https within 30 seconds"
}

api_status() {
  curl -s -o "${WORK}/api.body" -w '%{http_code}' --cacert "${CA}" --max-time 15 \
    "https://localhost:${VITE_PORT}/api/version" || true
}

# (a) Vite over https, proxying to the Go server with the Go server's own certificate as the anchor.
start_vite "${MIRROR}"
code="$(api_status)"
if [ "${code}" = "200" ] && grep -q '"display"' "${WORK}/api.body"; then
  pass "(a) https://localhost:${VITE_PORT}/api/version answered 200 through the verified proxy"
else
  fail "(a) /api/version through Vite answered '${code}', want 200 (Node may refuse the self-signed certificate as its own anchor)"
fi

# (c) No plain dev UI: a plain-text request to the TLS port gets no answer.
if curl -s -o /dev/null --max-time 5 "http://localhost:${VITE_PORT}/" 2>/dev/null; then
  fail "(c) http://localhost:${VITE_PORT}/ was answered; the dev UI must be https only"
else
  pass "(c) the plain http dev UI is refused"
fi

# (b) The proxy verifies its upstream: the dev leaf is not the Go server's certificate.
stop_vite
start_vite "${WORK}/dev-tls/cert.pem"
code="$(api_status)"
case "${code}" in
5??) pass "(b) an upstream whose certificate is not the trusted one is refused (${code})" ;;
*) fail "(b) a proxy trusting a different certificate answered '${code}', want a 5xx" ;;
esac

echo "dev-tls-check: ${passes} passed, ${failures} failed"
[ "${failures}" -eq 0 ] || die "${failures} check(s) failed"
