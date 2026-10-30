# SPDX-License-Identifier: AGPL-3.0-or-later
# SPDX-FileCopyrightText: 2026 The Coaming Authors
#
# The agent image. goreleaser (dockers_v2) builds it: it puts the binary for
# each platform at $TARGETPLATFORM/coaming-agent in the build context and copies
# the extra file agent/image/state/ to the same relative path. To build it by
# hand, lay a context out the same way (`make agent-image` does).
#
# The base is the same distroless "static" digest as the server image (no shell,
# no package manager, no libc), the root-capable variant and not :nonroot. The
# agent starts as root only to read a root-only Docker secret and to join the
# Docker socket's group, then drops itself to UID and GID 1000 and sets
# no-new-privileges (D-21, D-22, agent/internal/privdrop). That is why there is
# no USER line: the drop is the binary's job, and a swarm service cannot ask
# the engine for it, because `docker service create` has no --security-opt.
# The digest below is the multi-arch index of gcr.io/distroless/static-debian13 as resolved on
# 2026-10-02; bump it deliberately, never by tag, and together with the server image.
FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0

ARG TARGETPLATFORM

LABEL org.opencontainers.image.title="Coaming agent" \
      org.opencontainers.image.description="Connects one Docker host to a Coaming server; it dials out, so the host needs no inbound port" \
      org.opencontainers.image.source="https://github.com/ianglennon/coaming" \
      org.opencontainers.image.url="https://github.com/ianglennon/coaming" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.vendor="The Coaming Authors"

COPY $TARGETPLATFORM/coaming-agent /usr/local/bin/coaming-agent

# The state directory is created here, owned by 1000 and private, so that a new
# named volume mounted on it inherits that ownership and the agent needs no
# CAP_CHOWN (RESEARCH A1, A16). The extra file only makes the directory
# non-empty in the build context.
COPY --chown=1000:1000 --chmod=0700 agent/image/state/ /var/lib/coaming-agent/
VOLUME ["/var/lib/coaming-agent"]

# No HEALTHCHECK: a distroless image has no curl, and the server judges the
# agent by its connection (heartbeats), not by a probe inside the container.

ENTRYPOINT ["/usr/local/bin/coaming-agent"]
