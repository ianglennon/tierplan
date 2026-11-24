#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# Fetches the keepalived source the fourth image (keepalived/Dockerfile) is built
# from and proves it is the release that was reviewed (D-26, T-26-01).
#
# The tarball is the one keepalived.org publishes, never the GitHub tag archive,
# which has different bytes. It is written to build/keepalived/ under the
# repository root (gitignored). A copy already there with the right hash is kept
# and not downloaded again; one with another hash is removed and the script fails
# naming both hashes. The Dockerfile checks the same hash again inside the build
# stage, so the build is offline and uses these very bytes.
#
# Usage: scripts/keepalived-source.sh
# The path of the verified tarball is printed on standard output; messages go to
# standard error.
set -euo pipefail

VERSION=2.4.3
URL="https://www.keepalived.org/software/keepalived-${VERSION}.tar.gz"
SHA256=a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DIR="$ROOT/build/keepalived"
FILE="$DIR/keepalived-${VERSION}.tar.gz"

mkdir -p "$DIR"

have=""
if [ -f "$FILE" ]; then
  have=$(sha256sum "$FILE" | cut -d' ' -f1)
  if [ "$have" != "$SHA256" ]; then
    echo "keepalived-source: $FILE has sha256 $have, want $SHA256; removing it" >&2
    rm -f "$FILE"
    have=""
  fi
fi

if [ -z "$have" ]; then
  echo "keepalived-source: downloading $URL" >&2
  tmp=$(mktemp "$DIR/.download.XXXXXX")
  trap 'rm -f "$tmp"' EXIT
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output "$tmp" "$URL"
  got=$(sha256sum "$tmp" | cut -d' ' -f1)
  if [ "$got" != "$SHA256" ]; then
    echo "keepalived-source: the download has sha256 $got, want $SHA256" >&2
    exit 1
  fi
  mv "$tmp" "$FILE"
  trap - EXIT
fi

# One more check on the file that stays, the way the Dockerfile does it.
echo "$SHA256  $FILE" | sha256sum -c - >&2
echo "$FILE"
