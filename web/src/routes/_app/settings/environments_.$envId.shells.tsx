// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { ActiveShellsTab } from "@/features/environments/active-shells-tab";
import { usePermission } from "@/hooks/use-me";

// The Shells tab of an environment's page (D-37): every shell open on it, and the
// way to end one. It is for someone who holds shells.manage on this environment, an
// env Admin or an instance admin; the server checks the permission again on both of
// its routes, and answers 404 for an environment the caller has no grant on.
function EnvironmentShellsRoute() {
  const { envId } = Route.useParams();
  const allowed = usePermission("shells.manage", envId);
  if (!allowed) return <NoPermission />;
  return <ActiveShellsTab envId={envId} />;
}

export const Route = createFileRoute("/_app/settings/environments_/$envId/shells")({
  component: EnvironmentShellsRoute,
});
