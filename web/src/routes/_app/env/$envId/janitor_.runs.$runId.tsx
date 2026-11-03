// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { type RunSearchPatch, runSearchSchema } from "@/features/janitor/run-items-table";
import { RunPage } from "@/features/janitor/run-page";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function JanitorRun() {
  const { envId, runId } = Route.useParams();
  const search = Route.useSearch();
  const navigate = Route.useNavigate();
  const t = useT();
  useDocumentTitle(t("janitor.page.title"));
  return (
    <RunPage
      envId={envId}
      runId={runId}
      search={search}
      onSearchChange={(patch: RunSearchPatch) =>
        void navigate({ search: (prev) => ({ ...prev, ...patch }), replace: true })
      }
    />
  );
}

// The run page's search: which items to list (show), a search by name (q) and the item whose crash
// evidence is open (item). A value that does not fit is dropped, so a stale or hand-edited link opens
// the default (D-16, UI-SPEC "Screens").
export const Route = createFileRoute("/_app/env/$envId/janitor_/runs/$runId")({
  validateSearch: runSearchSchema,
  component: JanitorRun,
});
