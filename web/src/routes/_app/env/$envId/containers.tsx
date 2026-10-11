// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { ContainerList } from "@/features/containers/container-list";
import {
  type ContainerSearchPatch,
  containerSearchSchema,
} from "@/features/containers/list-search";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Containers() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("dashboard.lists.containers.title"));
  const allowed = usePermission("containers.read", envId);
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the filters, the search or the sort replaces the history entry,
  // so the back button leaves the page and does not walk through every key.
  function change(patch: ContainerSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) if (next[key] === undefined) delete next[key];
        return next;
      },
      replace: true,
    });
  }

  if (!allowed) return <NoPermission />;
  return <ContainerList envId={envId} search={search} onSearchChange={change} />;
}

export const Route = createFileRoute("/_app/env/$envId/containers")({
  validateSearch: containerSearchSchema,
  component: Containers,
});
