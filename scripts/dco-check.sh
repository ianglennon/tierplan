#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# DCO gate (D-69). Every non-merge commit in <base>..<head> must carry a
# Signed-off-by trailer. Usage: scripts/dco-check.sh <base> <head>
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <base> <head>" >&2
  exit 2
fi
base="$1"
head="$2"

# Capture the list first: a failure inside a process substitution does not stop the
# script, and an unresolvable range must fail the gate, never pass it.
if ! commits="$(git rev-list --no-merges "${base}..${head}")"; then
  echo "DCO FAIL: cannot list commits in ${base}..${head}" >&2
  exit 2
fi

failed=0
while IFS= read -r sha; do
  [ -n "$sha" ] || continue
  if ! git show -s --format=%B "$sha" | grep -Eq '^Signed-off-by: .+ <.+@.+>$'; then
    echo "DCO FAIL: $(git show -s --format='%h %s' "$sha")"
    failed=1
  fi
done <<<"${commits}"

exit "$failed"
