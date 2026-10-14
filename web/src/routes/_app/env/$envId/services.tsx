// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { ServiceList } from "@/features/services/service-list";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Services() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("services.list.title"));
  return <ServiceList envId={envId} />;
}

export const Route = createFileRoute("/_app/env/$envId/services")({
  component: Services,
});
