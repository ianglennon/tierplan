// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { StackResources } from "@/features/stacks/stack-resources";

// The Resources tab of a stack: everything it owns on its environment, under the stack's header and tab bar.
function StackResourcesRoute() {
  const { envId, stackId } = Route.useParams();
  return <StackResources envId={envId} stackId={stackId} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/resources")({
  component: StackResourcesRoute,
});
