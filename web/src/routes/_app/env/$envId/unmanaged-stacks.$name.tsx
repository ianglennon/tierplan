// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import { usePermission } from "@/hooks/use-me";

// The page of a stack TierPlan did not make: the frame (breadcrumb, header, notes, tab bar) around the tab the URL
// names. The name is the swarm namespace or the Compose project, built by the router from the address and never
// concatenated. The page is read-only: it needs stacks.read and offers no action on the stack (D-09, D-10).
function UnmanagedStackRoute() {
  const { envId, name } = Route.useParams();
  const allowed = usePermission("stacks.read", envId);
  if (!allowed) return <NoPermission />;
  return <StackDetailLayout variant="unmanaged" envId={envId} name={name} />;
}

export const Route = createFileRoute("/_app/env/$envId/unmanaged-stacks/$name")({
  component: UnmanagedStackRoute,
});
