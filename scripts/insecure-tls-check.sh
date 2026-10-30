#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Confines the switch that turns off Go's built-in TLS certificate verification
# to one reviewed file, agent/internal/connect/tls.go (D-06, T-02-32). Any other
# Go file that names the setting, in code, a test or a comment, fails the gate:
# the setting is dangerous enough that every use must be in the one place that
# replaces verification with a check.
#
# Usage: scripts/insecure-tls-check.sh [directory]
# The directory defaults to the repository root. Dependency and VCS directories
# are not scanned, nor .claude (local agent worktrees). Paths in the output are relative to the directory.
set -euo pipefail

ROOT="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
ALLOWED="agent/internal/connect/tls.go"
# Spelled in two halves so this file does not trip the gate it implements.
SETTING="Insecure""SkipVerify"

cd "${ROOT}"

bad=()
while IFS= read -r file; do
  file="${file#./}"
  [ "${file}" = "${ALLOWED}" ] && continue
  bad+=("${file}")
done < <(grep -rl --include='*.go' --exclude-dir=.git --exclude-dir=.claude --exclude-dir=node_modules --exclude-dir=vendor -e "${SETTING}" . 2>/dev/null | sort || true)

if [ "${#bad[@]}" -gt 0 ]; then
  for f in "${bad[@]}"; do
    echo "INSECURE TLS: ${f} names ${SETTING}; only ${ALLOWED} may" >&2
  done
  exit 1
fi
