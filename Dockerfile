# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# The runtime image. goreleaser (dockers_v2) builds it: it puts the binary for
# each platform at $TARGETPLATFORM/coaming in the build context. To build it by
# hand, put linux/amd64/coaming or linux/arm64/coaming next to this file.
#
# The base is distroless "static" (no shell, no package manager, no libc), the
# root-capable variant and not :nonroot. Coaming starts as root only to fix the
# ownership of /data and to join the Docker socket's group, then drops itself to
# UID and GID 1000 (D-17, internal/privdrop). That is why there is no USER line:
# `docker exec <container> coaming ...` must reach the same drop (RESEARCH A8).
# The digest below is the multi-arch index of gcr.io/distroless/static-debian13 as resolved on
# 2026-10-02; bump it deliberately, never by tag.
FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

ARG TARGETPLATFORM

LABEL org.opencontainers.image.title="Coaming" \
      org.opencontainers.image.description="A web-based manager for Docker Swarm clusters and standalone Docker hosts" \
      org.opencontainers.image.source="https://github.com/ianglennon/coaming" \
      org.opencontainers.image.url="https://github.com/ianglennon/coaming" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.vendor="The Coaming Authors"

COPY $TARGETPLATFORM/coaming /usr/local/bin/coaming

ENV COAMING_DATA_DIR=/data
# /data is created root-owned when the volume is new; privdrop hands it to 1000
# before the server touches it, which covers named volumes and bind mounts alike.
VOLUME ["/data"]
EXPOSE 7480

# A distroless image has no curl, so the binary probes itself. It switches to
# https by itself when Coaming serves TLS (D-86).
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/coaming", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/coaming"]
CMD ["serve"]
