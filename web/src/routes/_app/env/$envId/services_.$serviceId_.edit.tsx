// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";
import { lazy, Suspense } from "react";

import { NoPermission } from "@/components/full-page-state";
import { Skeleton } from "@/components/ui/skeleton";
import { usePermission } from "@/hooks/use-me";
import { z } from "@/lib/zod-config";

// The editor, with CodeMirror in it, is loaded when this route is opened and not before, so the
// lists and detail pages do not carry it (UI-SPEC "Design System").
const ServiceEditorPage = lazy(() =>
  import("@/features/service-editor/editor-page").then((m) => ({ default: m.ServiceEditorPage })),
);

// The editor's tab: the Form (the default) or the Spec. A value that is neither is dropped
// rather than refused, so an old or edited link still opens.
export const editSearchSchema = z.object({
  tab: z.enum(["form", "spec"]).optional().catch(undefined),
});

// Edit a service: the page that replaces the service's page while its settings are changed. The
// trailing underscore on the service's name in the file name keeps it outside the service's
// layout, so it has none of its tabs.
function ServiceEditRoute() {
  const { envId, serviceId } = Route.useParams();
  const { tab } = Route.useSearch();
  const navigate = Route.useNavigate();
  const allowed = usePermission("services.control", envId);
  if (!allowed) return <NoPermission />;
  return (
    <Suspense fallback={<Skeleton className="h-80 w-full" />}>
      <ServiceEditorPage
        envId={envId}
        serviceId={serviceId}
        tab={tab ?? "form"}
        onTabChange={(next) =>
          void navigate({ search: (prev) => ({ ...prev, tab: next }), replace: true })
        }
      />
    </Suspense>
  );
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId_/edit")({
  validateSearch: editSearchSchema,
  component: ServiceEditRoute,
});
