#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Keeps the ingress module small (D-42, T-23-07). The ingress node faces the
# internet and has no Docker socket, so the module may import only the standard
# library, github.com/ianglennon/tierplan/{ingress,proto}, github.com/coder/websocket
# and go.yaml.in/yaml/v3. Any other non-standard package in its dependency tree,
# direct or transitive, fails the gate: no Docker client, no ACME library, no yamux.
#
# Usage: scripts/ingress-deps-check.sh [--stdin]
# With no argument the import paths come from `go list -deps` over the ingress
# module. With --stdin they are read from standard input, one per line, so the
# gate's own fail-first cases can feed it a path without changing the module.
set -euo pipefail

ALLOWED='^(github\.com/ianglennon/tierplan/(ingress|proto)(/|$)|github\.com/coder/websocket(/|$)|go\.yaml\.in/yaml/v3(/|$))'

list_deps() {
  if [ "${1:-}" = "--stdin" ]; then
    cat
  else
    local root
    root="$(cd "$(dirname "$0")/.." && pwd)"
    go -C "$root/ingress" list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./...
  fi
}

# Capture the list first. A `go list` failure (module resolution, a broken replace,
# a toolchain error) then aborts here under set -e; reading it through a process
# substitution would hide the exit status and let the gate pass on a partial or
# empty list. An empty list is also a failure: nothing was checked.
paths="$(list_deps "${1:-}")"
if [ -z "${paths}" ]; then
  echo "INGRESS DEPS: no packages listed; the dependency graph was not checked" >&2
  exit 1
fi

bad=0
while IFS= read -r path; do
  [ -n "$path" ] || continue
  if ! printf '%s\n' "$path" | grep -Eq "$ALLOWED"; then
    echo "INGRESS DEPS: $path is not allowed in the ingress module (proto, coder/websocket, go.yaml.in/yaml/v3 and the standard library only)" >&2
    bad=1
  fi
done <<<"${paths}"

exit "$bad"
