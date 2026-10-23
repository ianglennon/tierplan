// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { DeployConsole } from "@/features/stack-deploy/deploy-console";

// The console of a deploy, under the stack's header: a view of a server-side job, so leaving the
// page does not stop the deploy and opening it again shows where it is (UI-SPEC "Deploy console").
function StackDeployRoute() {
  const { envId, stackId, jobId } = Route.useParams();
  // Keyed by the job: a roll back replaces this console with the next job's, which starts afresh.
  return <DeployConsole key={jobId} envId={envId} stackId={stackId} jobId={jobId} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/deploys/$jobId")({
  component: StackDeployRoute,
});
