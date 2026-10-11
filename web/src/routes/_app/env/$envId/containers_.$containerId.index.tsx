// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { Overview } from "@/features/container-detail/overview";

// The Overview tab of a container: the page's first tab and its start address.
function ContainerOverviewRoute() {
  const { envId, containerId } = Route.useParams();
  const { node } = Route.useSearch();
  return <Overview envId={envId} containerId={containerId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/containers_/$containerId/")({
  component: ContainerOverviewRoute,
});
