// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { StackJobView } from "@/features/stack-deploy/job-view";

// The console of a deploy, under the stack's header: a view of a server-side job, so leaving the
// page does not stop the deploy and opening it again shows where it is (UI-SPEC "Deploy
// console"). It starts as the job's steps and result; the live rows come with the console plan.
function StackDeployRoute() {
  const { envId, stackId, jobId } = Route.useParams();
  return <StackJobView envId={envId} stackId={stackId} jobId={jobId} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/deploys/$jobId")({
  component: StackDeployRoute,
});
