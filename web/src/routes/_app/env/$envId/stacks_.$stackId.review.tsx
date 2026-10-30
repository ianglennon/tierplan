// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { ReviewPage } from "@/features/stack-review/review-page";
import { usePermission } from "@/hooks/use-me";
import { z } from "@/lib/zod-config";

// The review's search: what is reviewed (the saved draft, a fresh digest for some services, or an
// earlier revision, or the deployed one again to restore the stack), the services an update names, as a
// comma list, and the revision a roll back restores. A value that does not fit is dropped rather than refused, so an old or edited link still
// opens the plain review (UI-SPEC "Screens").
export const reviewSearchSchema = z.object({
  mode: z.enum(["deploy", "update", "rollback", "restore"]).optional().catch(undefined),
  services: z.string().max(4000).optional().catch(undefined),
  revision: z.coerce.number().int().min(1).optional().catch(undefined),
});

// The names an update review looks up again, from the comma list in the address.
function servicesOf(list: string | undefined): string[] {
  return (list ?? "")
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s !== "");
}

// The Review step of a deploy, under the stack's header. It needs stacks.deploy. A deploy that
// starts replaces this page with its console, so Back returns to the editor and not to a review
// that is over.
function StackReviewRoute() {
  const { envId, stackId } = Route.useParams();
  const { mode, services, revision } = Route.useSearch();
  const navigate = Route.useNavigate();
  const allowed = usePermission("stacks.deploy", envId);
  if (!allowed) return <NoPermission />;
  // A roll back names the revision it restores; without one the link opens the plain review.
  const reviewed = mode === "rollback" && revision === undefined ? "deploy" : (mode ?? "deploy");
  return (
    <ReviewPage
      envId={envId}
      stackId={stackId}
      mode={reviewed}
      services={reviewed === "update" ? servicesOf(services) : []}
      revision={reviewed === "rollback" ? revision : undefined}
      onDeployed={(jobId) =>
        void navigate({
          to: "/env/$envId/stacks/$stackId/deploys/$jobId",
          params: { envId, stackId, jobId },
          replace: true,
        })
      }
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/stacks_/$stackId/review")({
  validateSearch: reviewSearchSchema,
  component: StackReviewRoute,
});
