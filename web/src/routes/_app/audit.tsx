// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { AuditLog } from "@/features/audit/audit-log";
import { auditSearchSchema } from "@/features/audit/filters";
import { useCanReadAudit, useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// /audit (D-43): the audit log, for an instance admin and for an env Admin, who
// sees the entries of the environments they administer (D-09). The filters are
// the validated search params, so a filtered view is a link. Anyone else gets
// the No permission page and no nav item; the server refuses the API calls as
// well.
function AuditPage() {
  const t = useT();
  useDocumentTitle(t("audit.tabTitle"));
  const allowed = useCanReadAudit();
  if (!allowed) return <NoPermission />;
  return <AuditLog />;
}

export const Route = createFileRoute("/_app/audit")({
  validateSearch: auditSearchSchema,
  component: AuditPage,
});
