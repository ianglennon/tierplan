# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# The git server of the end-to-end run (plan 09-33): one bare repository served over
# smart HTTP and TLS, so the From git golden path talks to a real git server with its
# own certificate authority and not to a stand-in. Test topology only, never shipped.
# Alpine's own packages, nothing downloaded from anywhere else.
FROM alpine:3

RUN apk add --no-cache git git-daemon lighttpd openssl

COPY lighttpd.conf /etc/lighttpd/git.conf
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY commit-change.sh /usr/local/bin/commit-change
RUN chmod 0755 /usr/local/bin/entrypoint.sh /usr/local/bin/commit-change

EXPOSE 443
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
