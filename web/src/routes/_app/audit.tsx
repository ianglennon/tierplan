// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { AuditLog } from "@/features/audit/audit-log";
import { auditSearchSchema } from "@/features/audit/filters";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// /audit (D-43): the audit log, for a user with audit.read. The filters are the
// validated search params, so a filtered view is a link. A user without the
// permission gets the No permission page and no nav item; the server refuses
// the API calls as well.
function AuditPage() {
  const t = useT();
  useDocumentTitle(t("audit.tabTitle"));
  const allowed = usePermission("audit.read");
  if (!allowed) return <NoPermission />;
  return <AuditLog />;
}

export const Route = createFileRoute("/_app/audit")({
  validateSearch: auditSearchSchema,
  component: AuditPage,
});
