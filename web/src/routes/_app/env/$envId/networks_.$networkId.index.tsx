// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NetworkOverview } from "@/features/networks/overview";

// The Overview tab of a network: the page's first tab and its start address.
function NetworkOverviewRoute() {
  const { envId, networkId } = Route.useParams();
  const { node } = Route.useSearch();
  return <NetworkOverview envId={envId} networkId={networkId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/networks_/$networkId/")({
  component: NetworkOverviewRoute,
});
