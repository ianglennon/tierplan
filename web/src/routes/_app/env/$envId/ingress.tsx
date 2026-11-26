// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { IngressLayout } from "@/features/ingress/ingress-layout";
import { usePermission } from "@/hooks/use-me";

// The Ingress page: the frame (title, tab bar) around the tab the URL names. Reading it needs ingress.read in
// this environment; anyone else sees the no-permission page, not a blank one.
function IngressRoute() {
  const { envId } = Route.useParams();
  const allowed = usePermission("ingress.read", envId);
  if (!allowed) return <NoPermission />;
  return <IngressLayout envId={envId} />;
}

export const Route = createFileRoute("/_app/env/$envId/ingress")({
  component: IngressRoute,
});
