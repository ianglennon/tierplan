#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Web interface SBOM (D-25, D-27, D-40). Writes the SPDX SBOM of the production npm
# dependencies of the SPA to the path given as the only argument, then proves it
# complete: the set of name@version pairs must equal pnpm's own licence list for
# the same dependencies (scripts/sbom-check.mjs pairs). The release archives and
# the server image carry this file (plan 23-10); `make web-sbom` runs this script.
#
#   scripts/web-sbom.sh build/sbom/tierplan-web.spdx.json
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
  echo "usage: scripts/web-sbom.sh <output.spdx.json>" >&2
  exit 2
fi

case "$1" in
  /*) out="$1" ;;
  *) out="${ROOT}/$1" ;;
esac

pnpm -C web install --frozen-lockfile
mkdir -p "$(dirname "${out}")"
pnpm -C web sbom --sbom-format spdx --prod --no-optional --out "${out}"

licences="$(mktemp)"
trap 'rm -f "${licences}"' EXIT
pnpm -C web licenses list --prod --json >"${licences}"

node scripts/sbom-check.mjs pairs --sbom "${out}" --licences "${licences}"
