// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import { usePermission } from "@/hooks/use-me";

// A stack's page: the frame (breadcrumb, header, tab bar) around the tab the URL names. The
// trailing underscore on the file name keeps the page outside the list route, so it replaces the
// list instead of rendering under it.
function StackDetailRoute() {
  const { envId, stackId } = Route.useParams();
  const allowed = usePermission("stacks.read", envId);
  if (!allowed) return <NoPermission />;
  return <StackDetailLayout envId={envId} stackId={stackId} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId")({
  component: StackDetailRoute,
});
