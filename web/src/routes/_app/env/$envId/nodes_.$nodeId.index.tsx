// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NodeOverview } from "@/features/nodes/overview";

// The Overview tab of a node: the page's first tab and its start address.
function NodeOverviewRoute() {
  const { envId, nodeId } = Route.useParams();
  return <NodeOverview envId={envId} nodeId={nodeId} />;
}

export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId/")({
  component: NodeOverviewRoute,
});
