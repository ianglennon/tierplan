// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";
import { lazy, Suspense } from "react";

import { NoPermission } from "@/components/full-page-state";
import { Skeleton } from "@/components/ui/skeleton";
import { usePermission } from "@/hooks/use-me";
import { editSearchSchema } from "@/routes/_app/env/$envId/services_.$serviceId_.edit";

// The editor, with CodeMirror in it, is loaded when this route is opened and not before, so the
// lists and detail pages do not carry it (UI-SPEC "Design System").
const ServiceEditorPage = lazy(() =>
  import("@/features/service-editor/editor-page").then((m) => ({ default: m.ServiceEditorPage })),
);

// Create a service (D-04): the same editor page as an edit, opened on the documented defaults. The
// trailing underscore on the services segment in the file name keeps it outside the services list's
// layout. Creating needs services.manage (Env Admin).
function ServiceCreateRoute() {
  const { envId } = Route.useParams();
  const { tab } = Route.useSearch();
  const navigate = Route.useNavigate();
  const allowed = usePermission("services.manage", envId);
  if (!allowed) return <NoPermission />;
  return (
    <Suspense fallback={<Skeleton className="h-80 w-full" />}>
      <ServiceEditorPage
        envId={envId}
        mode="create"
        tab={tab ?? "form"}
        onTabChange={(next) =>
          void navigate({ search: (prev) => ({ ...prev, tab: next }), replace: true })
        }
      />
    </Suspense>
  );
}

export const Route = createFileRoute("/_app/env/$envId/services_/new")({
  validateSearch: editSearchSchema,
  component: ServiceCreateRoute,
});
