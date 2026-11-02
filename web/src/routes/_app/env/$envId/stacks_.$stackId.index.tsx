// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { StackOverview } from "@/features/stacks/stack-overview";

// The Overview tab of a stack: the page's first tab and its start address.
function StackOverviewRoute() {
  const { envId, stackId } = Route.useParams();
  return <StackOverview envId={envId} stackId={stackId} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/")({
  component: StackOverviewRoute,
});
