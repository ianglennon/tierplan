// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { SecretDetailLayout } from "@/features/secrets/detail-layout";
import { usePermission } from "@/hooks/use-me";

// A secret's page: the frame (breadcrumb, header, tab bar) around the tab the URL names. The
// trailing underscore on the file name keeps the page outside the list route, so it replaces the
// list instead of rendering under it.
function SecretDetailRoute() {
  const { envId, secretId } = Route.useParams();
  const allowed = usePermission("secrets.read", envId);
  if (!allowed) return <NoPermission />;
  return <SecretDetailLayout envId={envId} secretId={secretId} />;
}

export const Route = createFileRoute("/_app/env/$envId/secrets_/$secretId")({
  component: SecretDetailRoute,
});
