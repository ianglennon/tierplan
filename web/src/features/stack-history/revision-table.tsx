// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Ellipsis } from "lucide-react";

import type { StackRevision } from "@/api/stack-history";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { TruncatedText } from "@/features/resources/resource-table";
import { ImageChangeSummary } from "@/features/stack-history/image-change-summary";
import { type HistoryRepo, RevisionCommit } from "@/features/stack-history/revision-commit";
import { RevisionKindCell, RevisionLink } from "@/features/stack-history/revision-kind-cell";
import { RevisionResultCell } from "@/features/stack-history/revision-result-cell";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface RevisionTableProps {
  envId: string;
  stackId: string;
  name: string;
  items: readonly StackRevision[];
  // The revision the detail below the table shows.
  selected: number | undefined;
  // Revisions that appeared while the tab was open: their rows are highlighted for a moment (P4 D-05).
  fresh: ReadonlySet<number>;
  // The repository a git stack follows. Its presence adds the Commit column; so does any revision that has a commit.
  repo?: HistoryRepo;
}

// The body shows 8 rows of 36px and scrolls inside, with the header row above them (UI-SPEC "Spacing
// Scale": History tab).
const BODY_MAX = "max-h-[324px]";

// RowActions is the row's menu: Open revision, and for an Operator Roll back to revision #n… on a revision that
// finished well and is not the deployed one. A failed or in-progress revision cannot be rolled back to (D-06) and
// the deployed one has nothing to roll back (D-51), so they offer Open revision only.
function RowActions({
  envId,
  stackId,
  item,
}: {
  envId: string;
  stackId: string;
  item: StackRevision;
}) {
  const t = useT();
  const needs = useNeeds("stacks.deploy", envId);
  const seq = item.seq;
  const canRollBack = needs.allowed && item.outcome === "good" && !item.deployed;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={t("stackHistory.table.rowMenu", { seq })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem asChild className="max-md:min-h-11">
          <Link
            to="/env/$envId/stacks/$stackId/history"
            params={{ envId, stackId }}
            search={{ rev: seq }}
            replace
          >
            {t("stackHistory.table.open", { seq })}
          </Link>
        </DropdownMenuItem>
        {canRollBack ? (
          <DropdownMenuItem asChild className="max-md:min-h-11">
            <Link
              to="/env/$envId/stacks/$stackId/review"
              params={{ envId, stackId }}
              search={{ mode: "rollback", revision: seq }}
            >
              {t("stackRollback.rowAction", { seq })}
            </Link>
          </DropdownMenuItem>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// WhoCell is who started a revision's deploy: a person, or "Auto-deploy" for the person whose authority an automatic deploy
// ran on (UI-SPEC "History additions", T-09-82). The server records that person as the revision's creator and says what
// started it in syncTrigger; the Tooltip names the trigger. Names are text.
function WhoCell({ item }: { item: StackRevision }) {
  const t = useT();
  if (!item.syncTrigger) {
    return item.createdBy ? (
      <TruncatedText>{item.createdBy}</TruncatedText>
    ) : (
      <span>{t("stackHistory.missing")}</span>
    );
  }
  const user = item.createdBy;
  const tip = !user
    ? t("stackHistory.table.auto.tipUnknown")
    : t(
        item.syncTrigger === "webhook"
          ? "stackHistory.table.auto.tipWebhook"
          : "stackHistory.table.auto.tipPoll",
        { user },
      );
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="flex flex-col leading-tight">
          <span>{t("stackHistory.table.auto.label")}</span>
          {user ? (
            <span className="max-w-64 truncate text-xs text-muted-foreground">
              {t("stackHistory.table.auto.for", { user })}
            </span>
          ) : null}
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
    </Tooltip>
  );
}

// RevisionTable lists the revisions of a stack, newest first. It is a native table with a caption, and the
// revision number is the link that selects a revision: it sets rev with replace, so selecting does not add
// a history entry per click. The selected row is marked with aria-current.
export function RevisionTable({
  envId,
  stackId,
  name,
  items,
  selected,
  fresh,
  repo,
}: RevisionTableProps) {
  const t = useT();
  const commits = repo !== undefined || items.some((i) => !!i.commit);
  return (
    <div className={cn("overflow-auto rounded-md border", BODY_MAX)}>
      <table className="w-full caption-bottom text-sm">
        <TableCaption className="sr-only">{t("stackHistory.table.caption", { name })}</TableCaption>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow className="hover:bg-muted">
            <TableHead scope="col">{t("stackHistory.table.columns.revision")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.kind")}</TableHead>
            {commits ? (
              <TableHead scope="col">{t("stackHistory.table.columns.commit")}</TableHead>
            ) : null}
            <TableHead scope="col">{t("stackHistory.table.columns.result")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.who")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.when")}</TableHead>
            <TableHead scope="col">{t("stackHistory.table.columns.images")}</TableHead>
            <TableHead scope="col" className="w-10">
              <span className="sr-only">{t("stackHistory.table.columns.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((item) => (
            <TableRow
              key={item.seq}
              aria-current={item.seq === selected ? "true" : undefined}
              data-state={item.seq === selected ? "selected" : undefined}
              className={cn(
                "h-9 max-md:h-11",
                fresh.has(item.seq) && "bg-muted motion-reduce:bg-transparent",
              )}
            >
              <TableCell>
                <span className="flex items-center gap-2">
                  <RevisionLink envId={envId} stackId={stackId} seq={item.seq} />
                  {item.deployed ? (
                    <Badge variant="outline">{t("stackHistory.table.deployed")}</Badge>
                  ) : null}
                </span>
              </TableCell>
              <TableCell>
                <RevisionKindCell item={item} envId={envId} stackId={stackId} />
              </TableCell>
              {commits ? (
                <TableCell>
                  <RevisionCommit item={item} repo={repo} />
                </TableCell>
              ) : null}
              <TableCell>
                <RevisionResultCell item={item} />
              </TableCell>
              <TableCell>
                <WhoCell item={item} />
              </TableCell>
              <TableCell>
                <RelativeTime value={item.createdAt} />
              </TableCell>
              <TableCell>
                <ImageChangeSummary item={item} />
              </TableCell>
              <TableCell className="text-end">
                <RowActions envId={envId} stackId={stackId} item={item} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </table>
    </div>
  );
}
