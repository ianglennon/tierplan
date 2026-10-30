// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { SecretOverview } from "@/features/secrets/overview";

// The Overview tab of a secret: the page's first tab and its start address.
function SecretOverviewRoute() {
  const { envId, secretId } = Route.useParams();
  return <SecretOverview envId={envId} secretId={secretId} />;
}

export const Route = createFileRoute("/_app/env/$envId/secrets_/$secretId/")({
  component: SecretOverviewRoute,
});
