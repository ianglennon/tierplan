// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { ConfigDetailLayout } from "@/features/configs/detail-layout";
import { usePermission } from "@/hooks/use-me";

// A config's page: the frame (breadcrumb, header, tab bar) around the tab the URL names. The
// trailing underscore on the file name keeps the page outside the list route, so it replaces the
// list instead of rendering under it.
function ConfigDetailRoute() {
  const { envId, configId } = Route.useParams();
  const allowed = usePermission("configs.read", envId);
  if (!allowed) return <NoPermission />;
  return <ConfigDetailLayout envId={envId} configId={configId} />;
}

export const Route = createFileRoute("/_app/env/$envId/configs_/$configId")({
  component: ConfigDetailRoute,
});
