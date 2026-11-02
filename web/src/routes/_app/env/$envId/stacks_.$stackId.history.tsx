// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { HistoryPage } from "@/features/stack-history/history-page";
import { z } from "@/lib/zod-config";

// The selected revision is in the address, so a link and a reload open the same one. A value that is not
// a whole number from 1 is dropped, and the page selects the deployed revision.
const historySearchSchema = z.object({
  rev: z.coerce.number().int().min(1).optional().catch(undefined),
});

// The History tab of a stack: every deploy of it as a revision, under the stack's header and tab bar.
function StackHistoryRoute() {
  const { envId, stackId } = Route.useParams();
  const { rev } = Route.useSearch();
  return <HistoryPage envId={envId} stackId={stackId} rev={rev} />;
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/history")({
  validateSearch: historySearchSchema,
  component: StackHistoryRoute,
});
