// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronDown, ChevronRight } from "lucide-react";
import { type ReactNode, useCallback, useMemo, useState } from "react";

import { Button } from "@/components/ui/button";
import { TableCell, TableRow } from "@/components/ui/table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface GroupHeaderRowProps {
  // The columns of the table it spans.
  colSpan: number;
  // The group's name, already translated when it is "Ungrouped".
  name: string;
  expanded: boolean;
  onToggle: () => void;
  // The id of the element that holds the group's rows.
  controls: string;
  // "{running} of {total} running", and the kind badge. Both are text.
  summary?: ReactNode;
  badge?: ReactNode;
  // The group's selection checkbox (plan 04-08). It sits before the chevron.
  checkbox?: ReactNode;
}

// GroupHeaderRow is the muted band that opens a group of rows (UI-SPEC "Group
// header rows"): 36px (44px on a phone), one cell across every column, a 32px
// chevron button that shows and hides the group, the name in 600 weight, then
// the count line and a badge. The names are Docker's, so they are text only.
export function GroupHeaderRow({
  colSpan,
  name,
  expanded,
  onToggle,
  controls,
  summary,
  badge,
  checkbox,
}: GroupHeaderRowProps) {
  const t = useT();
  const Chevron = expanded ? ChevronDown : ChevronRight;
  return (
    <TableRow data-slot="group-header-row" className="h-9 bg-muted hover:bg-muted max-md:h-11">
      <TableCell colSpan={colSpan} className="px-1 py-0">
        <div className="flex min-w-0 items-center gap-2">
          {checkbox}
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-8 max-md:size-11"
            aria-expanded={expanded}
            aria-controls={controls}
            aria-label={t(
              expanded ? "containers.list.group.collapse" : "containers.list.group.expand",
              {
                name,
              },
            )}
            onClick={onToggle}
          >
            <Chevron aria-hidden="true" />
          </Button>
          <span className="min-w-0 truncate text-sm font-semibold">{name}</span>
          {badge}
          {summary ? (
            <span className={cn("shrink-0 text-xs text-muted-foreground tabular-nums")}>
              {summary}
            </span>
          ) : null}
        </div>
      </TableCell>
    </TableRow>
  );
}

export interface GroupCollapse {
  // The keys of the groups that are folded away.
  collapsed: ReadonlySet<string>;
  toggle: (key: string) => void;
  collapseAll: (keys: readonly string[]) => void;
  expandAll: () => void;
}

// useGroupCollapse holds which groups are folded away. It lives in memory only
// (D-01): groups start expanded, and a reload opens them all again.
export function useGroupCollapse(): GroupCollapse {
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(() => new Set());
  const toggle = useCallback((key: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  }, []);
  const collapseAll = useCallback((keys: readonly string[]) => setCollapsed(new Set(keys)), []);
  const expandAll = useCallback(() => setCollapsed(new Set()), []);
  return useMemo(
    () => ({ collapsed, toggle, collapseAll, expandAll }),
    [collapsed, toggle, collapseAll, expandAll],
  );
}
