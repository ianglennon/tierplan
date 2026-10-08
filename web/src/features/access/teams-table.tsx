// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { EllipsisVertical } from "lucide-react";
import { type ReactNode, useState } from "react";

import { type Team, teamsQuery } from "@/api/teams";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CreateTeamDialog } from "@/features/access/create-team-dialog";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// How many skeleton rows keep the layout stable while loading (UI-SPEC).
const SKELETON_ROWS = 6;

// A row is 36px, and 44px under 768px for touch (D-64).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

interface Column {
  id: string;
  header: string;
  className?: string;
  cell: (team: Team) => ReactNode;
}

function RowActions({ team }: { team: Team }) {
  const t = useT();
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={t("teams.actionsFor", { team: team.name })}
        >
          <EllipsisVertical aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem asChild>
          <Link to="/settings/access/teams/$teamId" params={{ teamId: team.id }}>
            {t("teams.actions.open")}
          </Link>
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// TeamsTable is Settings > Access > Teams (D-25): Create team, the caption and a
// dense table of every team with its member count and what it grants. It renders
// team names as text only.
export function TeamsTable() {
  const t = useT();
  const query = useQuery(teamsQuery);
  const [createOpen, setCreateOpen] = useState(false);
  const rows = query.data ?? [];
  const loading = query.isPending;

  const columns: Column[] = [
    {
      id: "team",
      header: t("teams.columns.team"),
      cell: (team) => (
        <Link
          to="/settings/access/teams/$teamId"
          params={{ teamId: team.id }}
          className="min-w-0 rounded-sm text-accent-ink hover:underline"
        >
          <TruncatedText className="font-semibold">{team.name}</TruncatedText>
        </Link>
      ),
    },
    {
      id: "members",
      header: t("teams.columns.members"),
      className: "tabular-nums",
      cell: (team) => team.memberCount,
    },
    {
      id: "access",
      header: t("teams.columns.access"),
      cell: (team) =>
        team.grantCount === 0 ? (
          <span className="text-muted-foreground">{t("teams.noAccess")}</span>
        ) : (
          t("teams.grants", { count: team.grantCount })
        ),
    },
    {
      id: "actions",
      header: t("teams.columns.actions"),
      className: "w-12 text-right",
      cell: (team) => <RowActions team={team} />,
    },
  ];

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-3 md:flex-row md:items-end md:justify-end">
        <Button
          type="button"
          className="h-11 w-full md:h-8 md:w-auto"
          onClick={() => setCreateOpen(true)}
        >
          {t("teams.toolbar.create")}
        </Button>
      </div>

      {query.data ? (
        <p className="text-sm text-muted-foreground tabular-nums">
          {t("teams.caption", { count: rows.length })}
        </p>
      ) : null}

      <div className="max-h-[calc(100dvh-18rem)] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
        <table
          data-slot="table"
          aria-busy={loading || undefined}
          aria-label={t("teams.tableLabel")}
          className="w-full caption-bottom text-sm"
        >
          <TableHeader>
            <TableRow className={cn(ROW, "hover:bg-transparent")}>
              {columns.map((c) => (
                <TableHead
                  key={c.id}
                  className={cn(
                    "sticky top-0 z-10 h-12 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-12",
                    c.className,
                  )}
                >
                  {c.header}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading
              ? Array.from({ length: SKELETON_ROWS }, (_, i) => (
                  // Placeholder rows have no identity of their own.
                  // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                  <TableRow key={i} className={ROW}>
                    {columns.map((c) => (
                      <TableCell key={c.id} className={cn(CELL, c.className)}>
                        <Skeleton className="h-4 w-24" aria-hidden="true" />
                      </TableCell>
                    ))}
                  </TableRow>
                ))
              : rows.map((team) => (
                  <TableRow key={team.id} className={ROW}>
                    {columns.map((c) => (
                      <TableCell key={c.id} className={cn(CELL, c.className)}>
                        {c.cell(team)}
                      </TableCell>
                    ))}
                  </TableRow>
                ))}
          </TableBody>
        </table>
      </div>

      <CreateTeamDialog open={createOpen} onOpenChange={setCreateOpen} />
    </div>
  );
}
