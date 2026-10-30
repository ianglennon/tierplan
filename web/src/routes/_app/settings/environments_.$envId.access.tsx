// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { EnvAccessTab } from "@/features/environments/env-access-tab";
import { usePermission } from "@/hooks/use-me";

// The Access tab of an environment's page (D-25): who can use the environment and
// where their access comes from, and where an env Admin gives it (D-12). It is for
// someone who holds environments.access on this environment; the server checks the
// permission again on every route, and answers 404 for an environment the caller
// has no grant on.
function EnvironmentAccessRoute() {
  const { envId } = Route.useParams();
  const allowed = usePermission("environments.access", envId);
  if (!allowed) return <NoPermission />;
  return <EnvAccessTab envId={envId} />;
}

export const Route = createFileRoute("/_app/settings/environments_/$envId/access")({
  component: EnvironmentAccessRoute,
});
