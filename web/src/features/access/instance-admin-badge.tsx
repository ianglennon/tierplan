// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ShieldCheck } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";

// InstanceAdminBadge marks a user who holds instance authority (D-01). It never
// truncates: the text is the whole signal, the icon only decoration.
export function InstanceAdminBadge() {
  const t = useT();
  return (
    <Badge variant="outline" className="shrink-0">
      <ShieldCheck aria-hidden="true" />
      {t("users.instanceAdmin")}
    </Badge>
  );
}
