// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { UnmanagedOverview } from "@/features/stack-unmanaged/unmanaged-overview";

// The Overview tab of an unmanaged stack: the page's first tab and its start address.
function UnmanagedOverviewRoute() {
  const { envId, name } = Route.useParams();
  return <UnmanagedOverview envId={envId} name={name} />;
}

export const Route = createFileRoute("/_app/env/$envId/unmanaged-stacks/$name/")({
  component: UnmanagedOverviewRoute,
});
