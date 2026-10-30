// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { GeneratedCompose } from "@/features/stack-unmanaged/generated-compose";

// The Generated compose tab of an unmanaged stack: the compose file rebuilt from its running services, read only,
// under the stack's header, notes and tab bar. The page frame owns the gone state.
function UnmanagedComposeRoute() {
  const { envId, name } = Route.useParams();
  return <GeneratedCompose envId={envId} name={name} />;
}

export const Route = createFileRoute("/_app/env/$envId/unmanaged-stacks/$name/compose")({
  component: UnmanagedComposeRoute,
});
