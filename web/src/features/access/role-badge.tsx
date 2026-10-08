// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";

interface RoleBadgeProps {
  // viewer, operator or admin.
  role: string;
}

// RoleBadge is the role as an outline badge: "Admin", "Operator" or "Viewer"
// (UI-SPEC Component Inventory). The text is the whole signal, so it never
// truncates, and it is never the accent colour.
export function RoleBadge({ role }: RoleBadgeProps) {
  const t = useT();
  return (
    <Badge variant="outline" className="shrink-0">
      {t(`grants.role.${role}.label`)}
    </Badge>
  );
}
