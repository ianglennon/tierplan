#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Bayplan Authors
#
# Makes the throwaway certificate (it is its own certificate authority, as the test
# fixtures' is: the spec gives its PEM to Bayplan under Advanced) and the bare
# repository with one commit, then serves it. Everything is made afresh each time the
# container starts, so a run never sees the repository an earlier run left.
set -eu

mkdir -p /certs /srv/git /srv/www /work

# The names the run reaches it by: the compose service name, and the fixed address the
# compose file gives it.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
  -subj "/CN=gitserver/O=Bayplan end-to-end git server" \
  -addext "subjectAltName=DNS:gitserver,DNS:localhost,IP:172.30.0.11,IP:127.0.0.1" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,digitalSignature,keyCertSign" \
  -addext "extendedKeyUsage=serverAuth" \
  -keyout /certs/key.pem -out /certs/cert.pem 2>/dev/null
cat /certs/key.pem /certs/cert.pem > /certs/server.pem
chmod 0600 /certs/key.pem /certs/server.pem

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 HOME=/work
export GIT_AUTHOR_NAME="Bayplan E2E" GIT_AUTHOR_EMAIL=e2e@bayplan.invalid
export GIT_COMMITTER_NAME="Bayplan E2E" GIT_COMMITTER_EMAIL=e2e@bayplan.invalid

git init --quiet --bare -b main /srv/git/stacks.git
git init --quiet -b main /work/stacks
cd /work/stacks
mkdir -p stacks/web
# The image is the one the spec seeds into the managed engine's own registry; the engines
# of this topology have no route to any other.
cat > stacks/web/compose.yaml <<'YAML'
services:
  web:
    image: localhost:5000/e2e/gitweb:1
    command: ["sleep", "3600"]
    stop_grace_period: 1s
    environment:
      MODE: one
YAML
printf '# e2e stacks\n' > README.md
git add -A
git commit --quiet -m "web: mode one"
git push --quiet /srv/git/stacks.git main

exec lighttpd -D -f /etc/lighttpd/git.conf
