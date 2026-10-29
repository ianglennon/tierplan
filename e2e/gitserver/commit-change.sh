#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# commit-change <mode>: sets MODE in stacks/web/compose.yaml, commits it and pushes it to
# the bare repository the server serves, as a person pushing from a clone would, and prints the
# new head. The spec runs it with `docker compose exec`.
set -eu
mode="${1:?usage: commit-change <mode>}"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 HOME=/work
export GIT_AUTHOR_NAME="Bayplan E2E" GIT_AUTHOR_EMAIL=e2e@bayplan.invalid
export GIT_COMMITTER_NAME="Bayplan E2E" GIT_COMMITTER_EMAIL=e2e@bayplan.invalid
cd /work/stacks
sed -i "s/^      MODE: .*/      MODE: ${mode}/" stacks/web/compose.yaml
# Already at that mode: nothing to commit, and the head is what it was.
if git diff --quiet; then
  git rev-parse HEAD
  exit 0
fi
git commit --quiet -am "web: mode ${mode}"
git push --quiet /srv/git/stacks.git main
git rev-parse HEAD
