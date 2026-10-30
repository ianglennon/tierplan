// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { containerDetailQuery } from "@/api/container-detail";
import { StatsPanel } from "@/features/stats/stats-panel";

// The states in which the engine still has a cgroup to read: it is what the stream's own
// running flag means (a paused or restarting container is still running to the engine).
const RUNNING = new Set(["running", "paused", "restarting"]);

// The Stats tab of a container: its CPU, memory, network and block I/O, live. The page
// frame owns the gone state and the permission check; this draws the panel for the
// container the URL names. A container the page already knows is not running gets the
// not-running state at once and no stream.
function ContainerStatsRoute() {
  const { envId, containerId } = Route.useParams();
  const { node } = Route.useSearch();
  // The layout reads the detail too, so this is the cached answer; it names the surface.
  const detail = useQuery(containerDetailQuery(envId, containerId, node));
  return (
    <StatsPanel
      envId={envId}
      containerId={containerId}
      nodeId={node}
      name={detail.data?.name ?? containerId}
      nodeName={detail.data?.node}
      running={detail.data ? RUNNING.has(detail.data.state) : undefined}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/containers_/$containerId/stats")({
  component: ContainerStatsRoute,
});
