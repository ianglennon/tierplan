# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# The keepalived image: the virtual IP's VRRP daemon, one container per swarm node
# (D-26). goreleaser (dockers_v2) builds it: it puts the helper for each platform
# at $TARGETPLATFORM/tierplan-ingress in the build context, and the keepalived
# source tarball at build/keepalived/keepalived-2.4.3.tar.gz (scripts/keepalived-source.sh
# fetches and checks it). To build it by hand, lay a context out the same way
# (`make keepalived-image` does).
#
# keepalived is GPL-2.0-or-later, so it lives in this image alone and never in the
# server, agent or ingress images (VIP-06). The C stage is built from the tarball
# whose sha256 is checked here as well as by the script, so the bytes that were
# reviewed are the bytes compiled; it links statically, with LVS off, and only the
# binary and the licence text cross into the final stage. That stage is the same
# distroless "static" digest as Dockerfile, agent/Dockerfile and ingress/Dockerfile
# (bumped together): it has no shell, no busybox and no package manager.
#
# The helper (tierplan-ingress, the program the ingress image also carries) is
# PID 1: it renders keepalived's configuration, starts it as a child, reads what it
# reports and forwards SIGTERM. The container runs as root, because VRRP needs
# NET_ADMIN and NET_RAW and a raw socket in the host's network namespace (D-14); the
# service spec drops every capability but the few it names. There is no HEALTHCHECK:
# nothing in this image can run one except the helper (A-08).
#
# The Alpine build base is pinned by its multi-arch index digest, like the final
# base. Bump deliberately, never by tag. Its packages (the toolchain and a static
# OpenSSL for auth_hmac) float with the Alpine repository and stay in the discarded
# build stage.
#
# The image carries its evidence (D-27): the licence text at /licenses/keepalived/COPYING,
# the SBOM entry at /usr/share/tierplan/sbom/keepalived.spdx.json and a notice at
# /usr/share/doc/tierplan-keepalived/NOTICE.md naming the source, its hash and its licence.
# The build context also needs keepalived/image/ (sbom/ and doc/) beside the helper and
# the tarball.
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf AS build

RUN apk add --no-cache build-base linux-headers openssl-dev openssl-libs-static

COPY build/keepalived/keepalived-2.4.3.tar.gz /src/ka.tgz

RUN echo "a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632  /src/ka.tgz" | sha256sum -c - \
 && cd /src && tar xzf ka.tgz && cd keepalived-2.4.3 \
 && ./configure --prefix=/usr --sysconfdir=/etc \
      --disable-lvs --disable-iptables --disable-nftables --disable-systemd --disable-track-process \
      --enable-reproducible-build LDFLAGS=-static LIBS=-lcrypto \
 && make -j"$(nproc)" \
 && make DESTDIR=/out install-strip

# The SBOM entry for keepalived and OpenSSL (D-27) is a hand-written SPDX template
# with two placeholders: OpenSSL's version, read from the package installed above (it
# is what got linked), and a created time taken from SOURCE_DATE_EPOCH (the epoch when
# unset) so a rebuild is identical. The build fails if a placeholder is left. BuildKit's
# SBOM scanner merges this file with the helper's Go SBOM.
ARG SOURCE_DATE_EPOCH=0
COPY keepalived/image/sbom/keepalived.spdx.json.in /src/keepalived.spdx.json.in
RUN mkdir -p /out/sbom \
 && OPENSSL_VERSION="$(apk info -v openssl-libs-static | sed 's/^openssl-libs-static-//')" \
 && [ -n "${OPENSSL_VERSION}" ] \
 && CREATED="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)" \
 && sed -e "s/@OPENSSL_VERSION@/${OPENSSL_VERSION}/g" -e "s/@CREATED@/${CREATED}/g" /src/keepalived.spdx.json.in >/out/sbom/keepalived.spdx.json \
 && ! grep -Eq '@[A-Z_]+@' /out/sbom/keepalived.spdx.json

FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

ARG TARGETPLATFORM

LABEL org.opencontainers.image.title="TierPlan keepalived" \
      org.opencontainers.image.description="The virtual IP of a TierPlan swarm: keepalived 2.4.3 (VRRP with authenticated adverts) run by the TierPlan helper, one container per node" \
      org.opencontainers.image.source="https://github.com/ianglennon/tierplan" \
      org.opencontainers.image.url="https://github.com/ianglennon/tierplan" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later AND GPL-2.0-or-later" \
      org.opencontainers.image.vendor="The TierPlan Authors" \
      org.tierplan.keepalived.source="https://www.keepalived.org/software/keepalived-2.4.3.tar.gz" \
      org.tierplan.keepalived.sha256="a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632"

COPY --from=build /out/usr/sbin/keepalived /usr/sbin/keepalived
COPY --from=build /src/keepalived-2.4.3/COPYING /licenses/keepalived/COPYING
# A reproducible build leaves the build date out of the binary and has keepalived
# read its configure options from this file at start; without it keepalived prints
# "Unable to read build config options file" and exits 1, even for --version.
COPY --from=build /out/etc/keepalived/keepalived.config-opts /etc/keepalived/keepalived.config-opts
COPY --from=build /out/sbom/keepalived.spdx.json /usr/share/tierplan/sbom/keepalived.spdx.json
COPY keepalived/image/doc/NOTICE.md /usr/share/doc/tierplan-keepalived/NOTICE.md
COPY $TARGETPLATFORM/tierplan-ingress /usr/local/bin/tierplan-ingress

STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/tierplan-ingress", "vip-entrypoint"]
