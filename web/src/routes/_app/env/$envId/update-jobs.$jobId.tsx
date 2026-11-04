// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { UpdateConsole } from "@/features/updates/update-console";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The console of an image update of a plain service: a view of a server-side job, so leaving the page
// does not stop the update and opening it again shows where it is. Anyone who may read the
// environment may open it (UI-SPEC "Update console").
function UpdateJobRoute() {
  const { envId, jobId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("updates.console.documentTitle"));
  // Keyed by the job, so a link to another job starts afresh.
  return <UpdateConsole key={jobId} envId={envId} jobId={jobId} />;
}

export const Route = createFileRoute("/_app/env/$envId/update-jobs/$jobId")({
  component: UpdateJobRoute,
});
