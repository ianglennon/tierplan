// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { RunPage } from "@/features/janitor/run-page";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function JanitorRun() {
  const { envId, runId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("janitor.page.title"));
  return <RunPage envId={envId} runId={runId} />;
}

export const Route = createFileRoute("/_app/env/$envId/janitor_/runs/$runId")({
  component: JanitorRun,
});
