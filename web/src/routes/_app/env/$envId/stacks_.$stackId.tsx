// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute, useMatchRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { StackDetailLayout } from "@/features/stacks/stack-detail-layout";
import { usePermission } from "@/hooks/use-me";

// A stack's page: the frame (breadcrumb, header, tab bar) around the tab the URL names. The
// trailing underscore on the file name keeps the page outside the list route, so it replaces the
// list instead of rendering under it. On the Review and the console routes the frame shows the
// flow caption in place of the tab bar (UI-SPEC "Review").
function StackDetailRoute() {
  const { envId, stackId } = Route.useParams();
  const allowed = usePermission("stacks.read", envId);
  const matchRoute = useMatchRoute();
  if (!allowed) return <NoPermission />;
  const flow = matchRoute({ to: "/env/$envId/stacks/$stackId/review" })
    ? "review"
    : matchRoute({ to: "/env/$envId/stacks/$stackId/deploys/$jobId" })
      ? "deploy"
      : undefined;
  const editing = Boolean(matchRoute({ to: "/env/$envId/stacks/$stackId/editor" }));
  return <StackDetailLayout envId={envId} stackId={stackId} flow={flow} editing={editing} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId")({
  component: StackDetailRoute,
});
