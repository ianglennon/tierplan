# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The TierPlan Authors
#
# The runtime image. goreleaser (dockers_v2) builds it: it puts the binary for
# each platform at $TARGETPLATFORM/tierplan in the build context. To build it by
# hand, put linux/amd64/tierplan or linux/arm64/tierplan next to this file.
#
# The base is distroless "static" (no shell, no package manager, no libc), the
# root-capable variant and not :nonroot. TierPlan starts as root only to fix the
# ownership of /data and to join the Docker socket's group, then drops itself to
# UID and GID 1000 (D-17, internal/privdrop). That is why there is no USER line:
# `docker exec <container> tierplan ...` must reach the same drop (RESEARCH A8).
# The digest below is the multi-arch index of gcr.io/distroless/static-debian13 as resolved on
# 2026-10-02; bump it deliberately, never by tag.
FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

ARG TARGETPLATFORM

LABEL org.opencontainers.image.title="TierPlan" \
      org.opencontainers.image.description="A web-based manager for Docker Swarm clusters and standalone Docker hosts" \
      org.opencontainers.image.source="https://github.com/ianglennon/tierplan" \
      org.opencontainers.image.url="https://github.com/ianglennon/tierplan" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.vendor="The TierPlan Authors"

COPY $TARGETPLATFORM/tierplan /usr/local/bin/tierplan

ENV TIERPLAN_DATA_DIR=/data
# /data is created root-owned when the volume is new; privdrop hands it to 1000
# before the server touches it, which covers named volumes and bind mounts alike.
VOLUME ["/data"]
EXPOSE 7480

# A distroless image has no curl, so the binary probes itself. With
# TIERPLAN_TLS_SELF_SIGNED or the certificate-file variables it probes over
# https, verifying TierPlan's own certificate; otherwise it probes plain HTTP.
# /healthz answers on both schemes on the one port (D-86).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/tierplan", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/tierplan"]
CMD ["serve"]
