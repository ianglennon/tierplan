// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ShieldAlert } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { extraLabelKey } from "@/features/access/extra-permissions";
import { useT } from "@/i18n/use-t";

interface ExtraPermissionChipsProps {
  // The extra (dangerous) permissions held, as the registry names them.
  extras: readonly string[];
}

// ExtraPermissionChips lists the dangerous permissions an assignment holds, each
// an outline badge with a ShieldAlert icon and the permission's label as text.
// Neutral colour, never accent or a status colour. With none it says so in muted
// text, so an empty cell is never blank.
export function ExtraPermissionChips({ extras }: ExtraPermissionChipsProps) {
  const t = useT();
  if (extras.length === 0) {
    return <span className="text-muted-foreground">{t("grants.extra.none")}</span>;
  }
  return (
    <ul aria-label={t("grants.extra.legend")} className="flex flex-wrap gap-1">
      {extras.map((x) => (
        <li key={x}>
          <Badge variant="outline">
            <ShieldAlert aria-hidden="true" />
            {t(extraLabelKey(x))}
          </Badge>
        </li>
      ))}
    </ul>
  );
}
