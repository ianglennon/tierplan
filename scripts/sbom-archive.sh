#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# The SBOM of one release archive (D-25, D-27). The archive holds the Go binary and
# the web interface's SPDX SBOM (sbom/web.spdx.json). syft only merges that file into
# its own document through the SBOM cataloger, which is off by default, so it is
# selected here. The merged document is then gated on holding every npm package of the
# web SBOM (compared as a set, not a count) before anything is published: goreleaser
# runs its sboms step before it signs or publishes, so a non-zero exit from this
# script stops the release.
#
# goreleaser runs this as the sboms cmd with dist/ as the working directory, and
# passes the artifact and the document path relative to it:
#
#   scripts/sbom-archive.sh <artifact> <document>
set -euo pipefail

if [ "$#" -ne 2 ] || [ -z "$1" ] || [ -z "$2" ]; then
  echo "usage: sbom-archive.sh <artifact> <document>" >&2
  exit 2
fi

artifact="$1"
document="$2"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

syft "${artifact}" --select-catalogers +sbom-cataloger --output "spdx-json=${document}"

node "${root}/scripts/sbom-check.mjs" count \
  --web "${root}/build/sbom/tierplan-web.spdx.json" \
  --merged "${document}"
