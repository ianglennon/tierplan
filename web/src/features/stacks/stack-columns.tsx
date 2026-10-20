// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Ellipsis } from "lucide-react";

import type { StackItem } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { type Column, TruncatedText } from "@/features/resources/resource-table";
import { StackStateCell } from "@/features/stacks/stack-state-cell";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// A stack of a list with the environment it is on: the env list fills its own, the All stacks list
// each group's.
export interface StackRow extends StackItem {
  envId: string;
  envName: string;
}

export const stackRowKey = (s: StackRow) => `${s.envId}/${s.id}`;

interface RowActionsProps {
  row: StackRow;
  // Called as the row's menu opens and closes: a list with a menu open does not move.
  onOpenChange?: (open: boolean) => void;
}

// RowActions is the row menu of the stack lists: Open stack for everyone, Deploy… (which opens the
// review) for a user who may deploy and, for anyone else, disabled with the reason. The server decides
// either way.
function RowActions({ row, onOpenChange }: RowActionsProps) {
  const t = useT();
  const deploy = useNeeds("stacks.deploy", row.envId);
  return (
    <DropdownMenu onOpenChange={onOpenChange}>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={t("stackReview.rowMenu.label", { name: row.name })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem asChild className="max-md:min-h-11">
          <Link to="/env/$envId/stacks/$stackId" params={{ envId: row.envId, stackId: row.id }}>
            {t("stackReview.rowMenu.open")}
          </Link>
        </DropdownMenuItem>
        {deploy.allowed ? (
          <DropdownMenuItem asChild className="max-md:min-h-11">
            <Link
              to="/env/$envId/stacks/$stackId/review"
              params={{ envId: row.envId, stackId: row.id }}
            >
              {t("stackReview.deploy.action")}
            </Link>
          </DropdownMenuItem>
        ) : (
          <NeedsTooltip reason={deploy.reason}>
            <DropdownMenuItem className="max-md:min-h-11">
              {t("stackReview.deploy.action")}
            </DropdownMenuItem>
          </NeedsTooltip>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

interface StackColumnsOptions {
  // The All stacks list adds the Environment column after Name.
  withEnvironment?: boolean;
  onMenuOpenChange?: (row: StackRow, open: boolean) => void;
}

// stackColumns are the columns of the stack lists (UI-SPEC "Stack lists"): Name (a link, with a Draft
// badge while the draft holds changes that are not deployed), Environment on the global list, State,
// Services, Source, Last deployed and the row menu. Every word of a stack's or an environment's name
// renders as text.
export function stackColumns(
  t: ReturnType<typeof useT>,
  { withEnvironment, onMenuOpenChange }: StackColumnsOptions = {},
): Column<StackRow>[] {
  const columns: Column<StackRow>[] = [
    {
      id: "name",
      header: t("stackLists.columns.name"),
      className: "min-w-[224px]",
      cell: (s) => (
        <span className="flex min-w-0 items-center gap-2">
          <Link
            to="/env/$envId/stacks/$stackId"
            params={{ envId: s.envId, stackId: s.id }}
            className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <TruncatedText>{s.name}</TruncatedText>
          </Link>
          {s.hasDraft ? <Badge variant="outline">{t("stacks.draft")}</Badge> : null}
        </span>
      ),
    },
  ];
  if (withEnvironment) {
    columns.push({
      id: "environment",
      header: t("stackLists.columns.environment"),
      className: "w-48 min-w-48",
      cell: (s) => (
        <span className="flex min-w-0 items-center gap-2">
          <Link
            to="/env/$envId/stacks"
            params={{ envId: s.envId }}
            className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <TruncatedText>{s.envName}</TruncatedText>
          </Link>
          <Badge variant="outline">{t(`stackLists.target.${s.target}`)}</Badge>
        </span>
      ),
    });
  }
  columns.push(
    {
      id: "state",
      header: t("stackLists.columns.state"),
      className: "w-40 min-w-40",
      cell: (s) => (
        <StackStateCell state={s.state} running={s.servicesRunning} total={s.servicesTotal} />
      ),
    },
    {
      id: "services",
      header: t("stackLists.columns.services"),
      className: "w-24 min-w-24 text-end tabular-nums",
      cell: (s) =>
        s.servicesRunning !== undefined && s.servicesTotal !== undefined ? (
          <span>
            {t("stackLists.services", { running: s.servicesRunning, total: s.servicesTotal })}
          </span>
        ) : (
          <span className="text-muted-foreground">{t("stackLists.missing")}</span>
        ),
    },
    {
      id: "source",
      header: t("stackLists.columns.source"),
      className: "w-24 min-w-24",
      cell: (s) => <Badge variant="outline">{t(`stacks.source.${s.source}`)}</Badge>,
    },
    {
      id: "lastDeployed",
      header: t("stackLists.columns.lastDeployed"),
      className: "w-40 min-w-40",
      cell: (s) =>
        s.lastDeployedAt ? (
          <span className="flex flex-col leading-tight">
            <RelativeTime value={s.lastDeployedAt} />
            {s.lastDeployedBy ? (
              <span className="text-xs text-muted-foreground max-md:hidden">
                {t("stackLists.lastDeployedBy", { user: s.lastDeployedBy })}
              </span>
            ) : null}
          </span>
        ) : (
          <span className="text-muted-foreground">{t("stackLists.never")}</span>
        ),
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      cell: (s) => <RowActions row={s} onOpenChange={(open) => onMenuOpenChange?.(s, open)} />,
    },
  );
  return columns;
}
