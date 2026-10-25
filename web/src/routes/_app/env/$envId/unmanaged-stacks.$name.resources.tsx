// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { UnmanagedResources } from "@/features/stack-unmanaged/unmanaged-resources";

// The Resources tab of an unmanaged stack: what its services use, with the ownership preview, under the stack's
// header, notes and tab bar.
function UnmanagedResourcesRoute() {
  const { envId, name } = Route.useParams();
  return <UnmanagedResources envId={envId} name={name} />;
}

export const Route = createFileRoute("/_app/env/$envId/unmanaged-stacks/$name/resources")({
  component: UnmanagedResourcesRoute,
});
