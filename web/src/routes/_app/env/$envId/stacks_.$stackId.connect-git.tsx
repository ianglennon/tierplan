// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { GitSourcePage } from "@/features/git-source/git-source-page";
import { usePermission } from "@/hooks/use-me";

// The Use a git repository page of an inline or adopted stack (UI-SPEC "Convert page", D-24). It is a page of its own and
// not a tab of the stack's page: it has its own breadcrumb and footer. Anyone who may read stacks can open it; the button
// needs Admin on the environment and says so. The choices are in component state and the URL holds nothing but the stack.
function ConnectGitRoute() {
  const { envId, stackId } = Route.useParams();
  const allowed = usePermission("stacks.read", envId);
  if (!allowed) return <NoPermission />;
  return <GitSourcePage envId={envId} stackId={stackId} mode="convert" />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/connect-git")({
  component: ConnectGitRoute,
});
