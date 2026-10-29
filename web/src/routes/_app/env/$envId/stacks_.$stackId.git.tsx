// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { GitTab } from "@/features/stack-git/git-tab";

// The Git tab of a git stack: where it follows the repository and how it is kept in step. A stack that is not from a
// repository is sent to its Overview by the tab itself.
function StackGitRoute() {
  const { envId, stackId } = Route.useParams();
  return <GitTab envId={envId} stackId={stackId} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/git")({
  component: StackGitRoute,
});
