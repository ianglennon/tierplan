// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { DriftPage } from "@/features/stack-drift/drift-page";

// The drift page of a stack: what runs against the deployed revision. It sits under the stack's header and tab bar
// with no tab active, so a long diff keeps the page's own scroll and can be linked.
function StackDriftRoute() {
  const { envId, stackId } = Route.useParams();
  return <DriftPage envId={envId} stackId={stackId} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/drift")({
  component: StackDriftRoute,
});
