// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

import type { StackRevision } from "@/api/stack-history";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import {
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface RevisionTableProps {
  envId: string;
  stackId: string;
  name: string;
  items: readonly StackRevision[];
  // The revision the detail below the table shows.
  selected: number | undefined;
}

// The body shows 8 rows of 36px and scrolls inside, with the header row above them (UI-SPEC "Spacing
// Scale": History tab).
const BODY_MAX = "max-h-[324px]";

// RevisionTable lists the revisions of a stack, newest first. It is a native table with a caption, and the
// revision number is the link that selects a revision: it sets rev with replace, so selecting does not add
// a history entry per click.
export function RevisionTable({ envId, stackId, name, items, selected }: RevisionTableProps) {
  const t = useT();
  return (
    <div className={cn("overflow-auto rounded-md border", BODY_MAX)}>
      <table className="w-full caption-bottom text-sm">
        <TableCaption className="sr-only">{t("stackHistory.table.caption", { name })}</TableCaption>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow className="hover:bg-muted">
            <TableHead scope="col">{t("stackHistory.table.columns.revision")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.kind")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.result")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.who")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.when")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.images")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((item) => (
            <TableRow
              key={item.seq}
              aria-current={item.seq === selected ? "true" : undefined}
              data-state={item.seq === selected ? "selected" : undefined}
              className="h-9 max-md:h-11"
            >
              <TableCell>
                <span className="flex items-center gap-2">
                  <Link
                    to="/env/$envId/stacks/$stackId/history"
                    params={{ envId, stackId }}
                    search={{ rev: item.seq }}
                    replace
                    className="rounded-sm font-mono text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                  >
                    #{item.seq}
                  </Link>
                  {item.deployed ? (
                    <Badge variant="outline">{t("stackHistory.table.deployed")}</Badge>
                  ) : null}
                </span>
              </TableCell>
              <TableCell>{t(`stackHistory.kind.${item.origin}`)}</TableCell>
              <TableCell>{t(`stackHistory.result.${item.outcome}`)}</TableCell>
              <TableCell>
                {item.createdBy ? (
                  <TruncatedText>{item.createdBy}</TruncatedText>
                ) : (
                  t("stackHistory.missing")
                )}
              </TableCell>
              <TableCell>
                <RelativeTime value={item.createdAt} />
              </TableCell>
              <TableCell>
                {(item.images ?? []).length > 0
                  ? t("stackHistory.images.changed", { count: (item.images ?? []).length })
                  : item.pinned > 0
                    ? t("stackHistory.images.pinned", { count: item.pinned })
                    : t("stackHistory.images.none")}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </table>
    </div>
  );
}
