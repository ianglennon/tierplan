#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# SPDX header gate (D-68). Every tracked .go, .ts, .tsx and .sql file must carry
# both SPDX lines in its first 5 lines. Usage:
#   scripts/spdx-check.sh [file ...]     (default: every tracked source file)
# Exempt: generated files ("Code generated" and "DO NOT EDIT" in the first 5
# lines) and, for the default scan only, a short list of path prefixes. Files
# named explicitly on the command line are always checked, which is how the
# fail-first fixtures under scripts/testdata/ are exercised. The shadcn
# components are MIT code authored elsewhere and keep MIT attribution in the
# third-party notices.
set -euo pipefail

cd "$(dirname "$0")/.."

EXEMPT_PREFIXES=(
  "web/src/components/ui/"
  "web/src/api/schema.d.ts"
  "web/src/routeTree.gen.ts"
  "internal/store/sqlc/"
  "scripts/testdata/"
)

LICENSE_LINE="SPDX-License-Identifier: AGPL-3.0-or-later"
COPYRIGHT_LINE="SPDX-FileCopyrightText: 2026 The Bayplan Authors"

use_exemptions=1
if [ "$#" -gt 0 ]; then
  files=("$@")
  use_exemptions=0
else
  mapfile -t files < <(git ls-files '*.go' '*.ts' '*.tsx' '*.sql')
fi

failed=0
for f in "${files[@]}"; do
  exempt=0
  if [ "$use_exemptions" -eq 1 ]; then
    for prefix in "${EXEMPT_PREFIXES[@]}"; do
      case "$f" in
        "$prefix"*) exempt=1; break ;;
      esac
    done
  fi
  [ "$exempt" -eq 1 ] && continue
  [ -f "$f" ] || continue

  head5="$(head -n 5 -- "$f")"
  case "$head5" in
    *"Code generated"*"DO NOT EDIT"*) continue ;;
  esac
  if [[ "$head5" == *"$LICENSE_LINE"* && "$head5" == *"$COPYRIGHT_LINE"* ]]; then
    continue
  fi
  echo "SPDX missing: $f"
  failed=1
done

exit "$failed"
