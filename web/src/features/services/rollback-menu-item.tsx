// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { NeedsTooltip } from "@/components/needs-tooltip";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import type { Needs } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface RollbackMenuItemProps {
  // True when the swarm still holds the spec before the last update.
  hasPreviousSpec: boolean;
  // Whether the signed-in user may control services on the environment, and if not why not.
  needs: Needs;
  onSelect: () => void;
}

// RollbackMenuItem is "Roll back…" in a service's menu. A caller below Operator sees it unavailable
// with the reason as a tooltip (D-19). A service with no earlier version shows it unavailable with
// the reason as text under the label: a disabled menu item takes no pointer events, so a tooltip
// could not show it. The server decides either way.
export function RollbackMenuItem({ hasPreviousSpec, needs, onSelect }: RollbackMenuItemProps) {
  const t = useT();
  const label = t("services.list.menu.rollback");
  if (needs.allowed && !hasPreviousSpec) {
    return (
      <DropdownMenuItem disabled className="flex-col items-start gap-0 max-md:min-h-11">
        <span>{label}</span>
        <span className="text-xs text-muted-foreground">{t("services.rollback.noEarlier")}</span>
      </DropdownMenuItem>
    );
  }
  return (
    <NeedsTooltip reason={needs.reason}>
      <DropdownMenuItem
        className="max-md:min-h-11"
        disabled={!needs.allowed && !needs.reason}
        onSelect={() => {
          if (needs.allowed) onSelect();
        }}
      >
        {label}
      </DropdownMenuItem>
    </NeedsTooltip>
  );
}
