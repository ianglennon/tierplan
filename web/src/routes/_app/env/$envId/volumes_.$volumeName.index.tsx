// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { VolumeOverview } from "@/features/volumes/overview";

// The Overview tab of a volume: the page's first tab and its start address.
function VolumeOverviewRoute() {
  const { envId, volumeName } = Route.useParams();
  const { node } = Route.useSearch();
  return <VolumeOverview envId={envId} volumeName={volumeName} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/volumes_/$volumeName/")({
  component: VolumeOverviewRoute,
});
