// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Ellipsis } from "lucide-react";

import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// One button of the bar.
export interface BulkAction {
  id: string;
  label: string;
  onClick: () => void;
  // Why it cannot be used now: a missing permission, or no selected row it
  // applies to. The button stays in the tab order, aria-disabled, and says why.
  reason?: string;
  // Kill and Remove read in destructive text.
  destructive?: boolean;
}

interface BulkBarProps {
  // How many rows are selected.
  count: number;
  actions: readonly BulkAction[];
  onClear: () => void;
  // Offered when every visible eligible row is selected and more exist.
  selectAll?: { count: number; onSelect: () => void };
}

// How many buttons stay in the bar under 768px before the rest go in a menu.
const PHONE_BUTTONS = 2;

// BulkBar is the 48px bar that takes the caption's place while rows are
// selected (D-05, D-06): "{n} selected", the actions, Clear selection. It is a
// toolbar named "Bulk actions"; the count is a polite live region. Under 768px
// the first two actions show and the rest sit in a More menu.
export function BulkBar({ count, actions, onClear, selectAll }: BulkBarProps) {
  const t = useT();
  const rest = actions.slice(PHONE_BUTTONS);
  return (
    <div
      role="toolbar"
      aria-label={t("actions.bulk.label")}
      data-slot="bulk-bar"
      className="sticky top-12 z-20 flex min-h-12 flex-wrap items-center gap-2 rounded-lg bg-muted px-3 py-1.5"
    >
      <span role="status" aria-live="polite" className="text-sm font-semibold tabular-nums">
        {t("actions.bulk.selected", { count })}
      </span>
      {actions.map((a, i) => (
        <NeedsTooltip key={a.id} reason={a.reason}>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className={cn(
              "max-md:h-11",
              a.destructive && "text-destructive",
              i >= PHONE_BUTTONS && "max-md:hidden",
            )}
            onClick={a.onClick}
          >
            {a.label}
          </Button>
        </NeedsTooltip>
      ))}
      {rest.length > 0 ? (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11 md:hidden"
              aria-label={t("actions.bulk.more")}
            >
              <Ellipsis aria-hidden="true" />
              {t("actions.bulk.more")}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {rest.map((a) => (
              <NeedsTooltip key={a.id} reason={a.reason}>
                <DropdownMenuItem
                  className="max-md:min-h-11"
                  variant={a.destructive ? "destructive" : "default"}
                  onSelect={a.onClick}
                >
                  {a.label}
                </DropdownMenuItem>
              </NeedsTooltip>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      ) : null}
      <span className="ms-auto flex flex-wrap items-center gap-2">
        {selectAll ? (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="max-md:h-11"
            onClick={selectAll.onSelect}
          >
            {t("actions.bulk.selectAll", { count: selectAll.count })}
          </Button>
        ) : null}
        <Button type="button" variant="ghost" size="sm" className="max-md:h-11" onClick={onClear}>
          {t("actions.bulk.clear")}
        </Button>
      </span>
    </div>
  );
}
