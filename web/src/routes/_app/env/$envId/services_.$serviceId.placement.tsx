// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import {
  type PlacementSearchPatch,
  placementSearchSchema,
} from "@/features/placement/placement-search";
import { PlacementView } from "@/features/placement/placement-view";

// The Placement tab of a service (D-19): where its tasks run and where its rules let them run.
// Only the layout and the live-stats switch are in the address; the stack and service filters
// make no sense for one service.
const tabSearchSchema = placementSearchSchema.pick({ view: true, stats: true });

function ServicePlacementRoute() {
  const { envId, serviceId } = Route.useParams();
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change of layout replaces the history entry, as on the Placement page.
  function change(patch: PlacementSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });
  }

  return (
    <PlacementView envId={envId} serviceId={serviceId} search={search} onSearchChange={change} />
  );
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId/placement")({
  validateSearch: tabSearchSchema,
  component: ServicePlacementRoute,
});
