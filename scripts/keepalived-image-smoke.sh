#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Checks the keepalived image (`make keepalived-image` builds tierplan-keepalived:dev)
# for what its licence and supply-chain obligations need (VIP-06, D-26, D-27):
#
#   - /usr/sbin/keepalived runs and reports 2.4.3;
#   - the licence text (/licenses/keepalived/COPYING), the SBOM entry
#     (/usr/share/tierplan/sbom/keepalived.spdx.json) and the notice
#     (/usr/share/doc/tierplan-keepalived/NOTICE.md) are in the image with a size above
#     zero, and so is the helper (/usr/local/bin/tierplan-ingress);
#   - the image has no shell, no busybox and no package manager;
#   - the labels carry the tarball's URL and its sha256.
#
# The files are read from the listing of the archive `docker cp` makes of a container
# that was created and never started; only sizes and names are read, never the contents.
# Environment: IMAGE (default tierplan-keepalived:dev). Needs Docker and tar. Removes
# what it creates; the container carries the label tierplan.ci.owner
# (TIERPLAN_CI_OWNER, local-<pid> when unset) so `make ci-clean` removes what a
# cancelled CI job left, and its name carries this process id, so two runs on one
# runner do not collide.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${IMAGE:-tierplan-keepalived:dev}"
WANT_SOURCE="https://www.keepalived.org/software/keepalived-2.4.3.tar.gz"
WANT_SHA256="a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632"

NAME="tierplan-keepalived-smoke-$$"
OWNER_LABEL="tierplan.ci.owner=${TIERPLAN_CI_OWNER:-local-$$}"
WORK="$(mktemp -d)"

cleanup() {
  docker rm -f "${NAME}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

fail() {
  echo "KEEPALIVED IMAGE SMOKE FAIL: $1" >&2
  exit 1
}

pass() { echo "PASS: $1"; }

docker image inspect "${IMAGE}" >/dev/null 2>&1 \
  || fail "${IMAGE} not found; run make keepalived-image first"

# 1. The binary runs and is the pinned version. Its output is one version line.
version="$(docker run --rm --label "${OWNER_LABEL}" --name "${NAME}-version" --entrypoint /usr/sbin/keepalived "${IMAGE}" --version 2>&1 | head -n 1)" \
  || fail "keepalived --version did not run"
case "${version}" in
  *"v2.4.3"*) pass "keepalived reports v2.4.3" ;;
  *) fail "keepalived --version reports '${version}', want v2.4.3" ;;
esac

# 2. The files, from the listing of the archive docker cp makes. A listing line reads
#    "<mode> <owner> <size> <date> <time> <name>"; names are normalised to start at /.
docker create --label "${OWNER_LABEL}" --name "${NAME}" "${IMAGE}" >/dev/null \
  || fail "could not create a container from ${IMAGE}"
docker cp "${NAME}:/" - | tar -tvf - >"${WORK}/listing" \
  || fail "could not list the image filesystem"
awk '{ name = $6; sub(/^\.?\/?/, "/", name); print $3, name }' "${WORK}/listing" >"${WORK}/sizes"

for f in /licenses/keepalived/COPYING \
         /usr/share/tierplan/sbom/keepalived.spdx.json \
         /usr/share/doc/tierplan-keepalived/NOTICE.md \
         /usr/local/bin/tierplan-ingress \
         /usr/sbin/keepalived; do
  size="$(awk -v f="${f}" '$2 == f { print $1; exit }' "${WORK}/sizes")"
  [ -n "${size}" ] || fail "${f} is missing from the image"
  [ "${size}" -gt 0 ] || fail "${f} is empty"
  pass "${f} is in the image (${size} bytes)"
done

# 3. No shell, no busybox, no package manager: no entry of these names anywhere.
for pat in '/bin/sh$' '/bin/ash$' '/bin/bash$' '/busybox$' '/sbin/apk$' '/usr/bin/apk$'; do
  hit="$(awk '{ print $2 }' "${WORK}/sizes" | grep -E "${pat}" | head -n 1 || true)"
  [ -z "${hit}" ] || fail "the image holds ${hit}; it must have no shell, busybox or package manager"
done
pass "the image has no shell, no busybox and no package manager"

# 4. The labels that name the source.
source_label="$(docker image inspect -f '{{index .Config.Labels "org.tierplan.keepalived.source"}}' "${IMAGE}")"
[ "${source_label}" = "${WANT_SOURCE}" ] || fail "label org.tierplan.keepalived.source is '${source_label}', want ${WANT_SOURCE}"
hash_label="$(docker image inspect -f '{{index .Config.Labels "org.tierplan.keepalived.sha256"}}' "${IMAGE}")"
[ "${hash_label}" = "${WANT_SHA256}" ] || fail "label org.tierplan.keepalived.sha256 is '${hash_label}', want ${WANT_SHA256}"
pass "the labels carry the tarball URL and its sha256"

echo "keepalived image smoke: ok"
