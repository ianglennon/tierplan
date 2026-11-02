// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { EllipsisVertical } from "lucide-react";
import { type ReactNode, useState } from "react";

import { type Team, teamsQuery } from "@/api/teams";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CreateTeamDialog } from "@/features/access/create-team-dialog";
import { DeleteTeamDialog } from "@/features/access/delete-team-dialog";
import { RenameTeamDialog } from "@/features/access/rename-team-dialog";
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

// byName is the order of the table: alphabetical ignoring case and accents, with
// the ID breaking a tie so the order never flickers. The server sorts too; this
// keeps the table in order while a rename is still on its way back.
function byName(a: Team, b: Team): number {
  return (
    a.name.localeCompare(b.name, undefined, { sensitivity: "base" }) || a.id.localeCompare(b.id)
  );
}

interface RowActionsProps {
  team: Team;
  onRename: (team: Team) => void;
  onDelete: (team: Team) => void;
}

function RowActions({ team, onRename, onDelete }: RowActionsProps) {
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
        <DropdownMenuItem onSelect={() => onRename(team)}>
          {t("teams.actions.rename")}
        </DropdownMenuItem>
        <DropdownMenuItem variant="destructive" onSelect={() => onDelete(team)}>
          {t("teams.actions.delete")}
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
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [renameFor, setRenameFor] = useState<Team | null>(null);
  const [deleteFor, setDeleteFor] = useState<Team | null>(null);
  const rows = [...(query.data ?? [])].sort(byName);
  const failed = query.isError && !query.data;
  const loading = query.isPending;
  const empty = query.data !== undefined && rows.length === 0;
  const refresh = () => void queryClient.invalidateQueries({ queryKey: teamsQuery.queryKey });

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
      cell: (team) => <RowActions team={team} onRename={setRenameFor} onDelete={setDeleteFor} />,
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

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("teams.loadFailed")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("teams.loadFailedBody")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("teams.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {empty ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyTitle>{t("teams.empty.title")}</EmptyTitle>
            <EmptyDescription>{t("teams.empty.body")}</EmptyDescription>
          </EmptyHeader>
          <Button type="button" className="h-11 md:h-8" onClick={() => setCreateOpen(true)}>
            {t("teams.toolbar.create")}
          </Button>
        </Empty>
      ) : null}

      {!failed && !empty ? (
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
      ) : null}

      <CreateTeamDialog open={createOpen} onOpenChange={setCreateOpen} />
      <RenameTeamDialog
        team={renameFor}
        onClose={() => setRenameFor(null)}
        onSaved={() => {
          setRenameFor(null);
          refresh();
        }}
        onStale={refresh}
      />
      <DeleteTeamDialog
        team={deleteFor}
        onClose={() => setDeleteFor(null)}
        onDone={() => {
          setDeleteFor(null);
          refresh();
        }}
        onStale={refresh}
      />
    </div>
  );
}
