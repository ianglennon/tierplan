// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { containerDetailQuery } from "@/api/container-detail";
import { LogViewer } from "@/features/logs/log-viewer";

// The Logs tab of a container: its log, live. The page frame owns the gone state and
// the permission check; this draws the viewer for the container the URL names.
function ContainerLogsRoute() {
  const { envId, containerId } = Route.useParams();
  const { node } = Route.useSearch();
  // The layout reads the detail too, so this is the cached answer; it names the surface.
  const detail = useQuery(containerDetailQuery(envId, containerId, node));
  return (
    <LogViewer
      envId={envId}
      containerId={containerId}
      nodeId={node}
      name={detail.data?.name ?? containerId}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/containers_/$containerId/logs")({
  component: ContainerLogsRoute,
});
