# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# The ingress image. goreleaser (dockers_v2) builds it: it puts the supervisor
# for each platform at $TARGETPLATFORM/tierplan-ingress in the build context and
# copies the extra files ingress/image/state/ and ingress/image/doc/ to the same
# relative paths. To build it by hand, lay a context out the same way
# (`make ingress-image` does).
#
# Two programs live here: Traefik, copied out of the official image by digest,
# and the supervisor tierplan-ingress, which is PID 1, starts Traefik as a child
# and keeps the node's bundle cache (D-02, D-03: one container per node).
#
# The image starts non-root, as UID and GID 1000, and nothing in the binary drops
# privilege (D-05). It adds no capability and gets no Docker socket (D-06): the
# supervisor sets no-new-privileges on itself, which Traefik inherits. The
# volume directory below is created owned by 1000 and private so that a new named
# volume inherits it and the supervisor needs no chown.
#
# The Traefik reference is pinned here and in internal/ingressrender/traefiktest,
# and scripts/ingress-pin-check.sh keeps the two equal (D-08). The base is the
# same distroless "static" digest as Dockerfile and agent/Dockerfile; the three
# are bumped together and the same script checks they agree. Both digests are
# multi-arch indexes: buildx picks the platform from them. Bump deliberately,
# never by tag.
FROM traefik:v3.7.19@sha256:aa942a00bce033c909bf1f7e861d93761be4b31a035f10d2410d33e38a8f41cf AS traefik

FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

ARG TARGETPLATFORM

LABEL org.opencontainers.image.title="TierPlan ingress" \
      org.opencontainers.image.description="One ingress node: Traefik run by a supervisor that fetches its routes and certificates from a TierPlan server over an outbound channel" \
      org.opencontainers.image.source="https://github.com/ianglennon/tierplan" \
      org.opencontainers.image.url="https://github.com/ianglennon/tierplan" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later AND MIT" \
      org.opencontainers.image.vendor="The TierPlan Authors"

COPY --from=traefik /usr/local/bin/traefik /usr/local/bin/traefik
COPY $TARGETPLATFORM/tierplan-ingress /usr/local/bin/tierplan-ingress

# Traefik's official image ships no licence file; these are its MIT licence and a
# notice about the third-party software in this image (D-34).
COPY ingress/image/doc/ /usr/share/doc/traefik/

# The node's root: cache, certificates and Traefik's state. Owned by 1000 and
# private; the extra file only makes the directory non-empty in the build context.
COPY --chown=1000:1000 --chmod=0700 ingress/image/state/ /var/lib/tierplan-ingress/

USER 1000:1000
VOLUME ["/var/lib/tierplan-ingress"]
EXPOSE 8080 8443
STOPSIGNAL SIGTERM

# A distroless image has no curl, so the probe is the supervisor itself: it asks
# the readiness endpoint on 8083 and exits 0 on a 200. --start-interval is not
# used (Engine 24 does not know it); Phase 24 sets the same four values on the
# swarm service. A test ties these to proto/ingressbundle.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10m --retries=3 CMD ["/usr/local/bin/tierplan-ingress", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/tierplan-ingress"]
