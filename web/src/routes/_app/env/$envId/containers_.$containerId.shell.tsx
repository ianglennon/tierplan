// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { containerDetailQuery } from "@/api/container-detail";
import { rowOf } from "@/features/container-detail/header-actions";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { ShellPanel } from "@/features/terminal/shell-panel";

// The Shell tab of a container: a terminal inside it. The page frame owns the gone
// state; this draws the launcher and, once a session is open, the terminal. The tab is
// shown to everyone who can read the container, and the permission to open a shell is
// checked by the panel and by the server.
function ContainerShellRoute() {
  const { envId, containerId } = Route.useParams();
  const { node } = Route.useSearch();
  // The layout reads the detail too, so this is the cached answer.
  const detail = useQuery(containerDetailQuery(envId, containerId, node));
  const isSwarm = useEnvIsSwarm(envId);
  return (
    <ShellPanel
      envId={envId}
      containerId={containerId}
      nodeId={node ?? detail.data?.nodeId}
      name={detail.data?.name ?? containerId}
      node={isSwarm ? (detail.data?.node ?? undefined) : undefined}
      running={detail.data ? detail.data.state === "running" : undefined}
      row={detail.data ? rowOf(detail.data) : undefined}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/containers_/$containerId/shell")({
  component: ContainerShellRoute,
});
