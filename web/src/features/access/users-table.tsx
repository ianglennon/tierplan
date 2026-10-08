// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { EllipsisVertical } from "lucide-react";
import { type ReactNode, useState } from "react";

import {
  enableUser,
  type OneTimeLink,
  type UserSummary,
  unlockUser,
  usersQuery,
} from "@/api/users";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { CreateUserDialog } from "@/features/access/create-user-dialog";
import { DeleteUserDialog } from "@/features/access/delete-user-dialog";
import { DisableUserDialog } from "@/features/access/disable-user-dialog";
import { InstanceAdminBadge } from "@/features/access/instance-admin-badge";
import { OneTimeLinkDialog } from "@/features/access/one-time-link-dialog";
import { RegenerateInviteDialog } from "@/features/access/regenerate-invite-dialog";
import { UserStatusBadge } from "@/features/access/user-status-badge";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
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
  cell: (user: UserSummary) => ReactNode;
}

// matches says whether a user answers the search: a case-insensitive substring of
// the username, display name or email.
function matches(user: UserSummary, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return [user.username, user.displayName, user.email].some((v) => v.toLowerCase().includes(q));
}

// canRegenerate says whether a user is still waiting on an invite: only then is
// there a link to replace.
function canRegenerate(user: UserSummary): boolean {
  return user.status === "invite_pending" || user.status === "invite_expired";
}

interface RowActionsProps {
  user: UserSummary;
  onRegenerate: (user: UserSummary) => void;
  onDisable: (user: UserSummary) => void;
  onEnable: (user: UserSummary) => void;
  onUnlock: (user: UserSummary) => void;
  onDelete: (user: UserSummary) => void;
}

// RowActions is the "Actions for {username}" menu: Open always, Regenerate
// invite link while an invite is outstanding, Unlock while the account is
// locked, Disable or Enable, and Delete user…. For the last active instance
// admin, Disable and Delete are shown but unavailable, with the reason, because
// the server would refuse.
function RowActions({
  user,
  onRegenerate,
  onDisable,
  onEnable,
  onUnlock,
  onDelete,
}: RowActionsProps) {
  const t = useT();
  const guard = user.lastInstanceAdmin ? t("needs.lastAdmin") : undefined;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={t("users.actionsFor", { username: user.username })}
        >
          <EllipsisVertical aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem asChild>
          <Link to="/settings/access/users/$userId" params={{ userId: user.id }}>
            {t("users.actions.open")}
          </Link>
        </DropdownMenuItem>
        {canRegenerate(user) ? (
          <DropdownMenuItem onSelect={() => onRegenerate(user)}>
            {t("users.actions.regenerate")}
          </DropdownMenuItem>
        ) : null}
        {user.status === "locked" ? (
          <DropdownMenuItem onSelect={() => onUnlock(user)}>
            {t("users.actions.unlock")}
          </DropdownMenuItem>
        ) : null}
        {user.status === "disabled" ? (
          <DropdownMenuItem onSelect={() => onEnable(user)}>
            {t("users.actions.enable")}
          </DropdownMenuItem>
        ) : (
          <NeedsTooltip reason={guard}>
            <DropdownMenuItem onSelect={() => onDisable(user)}>
              {t("users.actions.disable")}
            </DropdownMenuItem>
          </NeedsTooltip>
        )}
        <NeedsTooltip reason={guard}>
          <DropdownMenuItem variant="destructive" onSelect={() => onDelete(user)}>
            {t("users.actions.delete")}
          </DropdownMenuItem>
        </NeedsTooltip>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// An invite link just made, held here until its dialog is closed and not a
// moment longer.
interface IssuedLink {
  username: string;
  link: OneTimeLink;
}

// UsersTable is Settings > Access > Users (D-25): a search field and Create user,
// the caption, and a dense table of every user with their status. The list is
// searched in the browser and never paged. It renders names, display names and
// emails as text only.
export function UsersTable() {
  const t = useT();
  const query = useQuery(usersQuery);
  const [search, setSearch] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const queryClient = useQueryClient();
  const [regenerateFor, setRegenerateFor] = useState<UserSummary | null>(null);
  const [issued, setIssued] = useState<IssuedLink | null>(null);
  const [disableFor, setDisableFor] = useState<UserSummary | null>(null);
  const [deleteFor, setDeleteFor] = useState<UserSummary | null>(null);

  const all = query.data ?? [];
  const failed = query.isError && !query.data;
  const loading = query.isPending;
  const rows = all.filter((u) => matches(u, search));
  const noMatch = !loading && !failed && search.trim() !== "" && rows.length === 0;

  const columns: Column[] = [
    {
      id: "user",
      header: t("users.columns.user"),
      cell: (u) => (
        <div className="flex min-w-0 flex-col">
          <div className="flex min-w-0 items-center gap-2">
            <Link
              to="/settings/access/users/$userId"
              params={{ userId: u.id }}
              className="min-w-0 rounded-sm text-accent-ink hover:underline"
            >
              <TruncatedText className="font-semibold">{u.username}</TruncatedText>
            </Link>
            {u.instanceAdmin ? <InstanceAdminBadge /> : null}
          </div>
          {u.displayName ? (
            <TruncatedText className="text-xs text-muted-foreground">{u.displayName}</TruncatedText>
          ) : null}
        </div>
      ),
    },
    {
      id: "status",
      header: t("users.columns.status"),
      cell: (u) => <UserStatusBadge status={u.status} lockedUntil={u.lockedUntil} />,
    },
    {
      id: "teams",
      header: t("users.columns.teams"),
      className: "tabular-nums",
      cell: (u) => t("users.teamCount", { count: u.teamCount }),
    },
    {
      id: "lastSignIn",
      header: t("users.columns.lastSignIn"),
      cell: (u) =>
        u.lastLoginAt ? (
          <RelativeTime value={u.lastLoginAt} />
        ) : (
          <span className="text-muted-foreground">{t("users.neverSignedIn")}</span>
        ),
    },
    {
      id: "actions",
      header: t("users.columns.actions"),
      className: "w-12 text-right",
      cell: (u) => (
        <RowActions
          user={u}
          onRegenerate={setRegenerateFor}
          onDisable={setDisableFor}
          onEnable={(user) => void enable(user)}
          onUnlock={(user) => void unlock(user)}
          onDelete={setDeleteFor}
        />
      ),
    },
  ];

  const refresh = () => void queryClient.invalidateQueries({ queryKey: usersQuery.queryKey });

  async function unlock(user: UserSummary) {
    const result = await unlockUser(user.id);
    if (result.kind === "ok") toastSuccess(t("userpage.unlocked", { username: user.username }));
    else toastError(t("userpage.unlockFailed", { username: user.username }));
    refresh();
  }

  async function enable(user: UserSummary) {
    const result = await enableUser(user.id);
    if (result.kind === "ok") toastSuccess(t("userpage.enabled", { username: user.username }));
    else toastError(t("userpage.enableFailed", { username: user.username }));
    refresh();
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-3 md:flex-row md:items-end md:justify-between">
        <div className="flex flex-col gap-1.5 md:w-80">
          <Label htmlFor="users-search" className="text-sm text-muted-foreground">
            {t("users.toolbar.searchLabel")}
          </Label>
          <Input
            id="users-search"
            type="search"
            value={search}
            placeholder={t("users.toolbar.searchPlaceholder")}
            autoComplete="off"
            className="h-11 md:h-8"
            onChange={(e) => setSearch(e.target.value)}
          />
        </div>
        <Button
          type="button"
          className="h-11 w-full md:h-8 md:w-auto"
          onClick={() => setCreateOpen(true)}
        >
          {t("users.toolbar.create")}
        </Button>
      </div>

      {query.data ? (
        <p className="text-sm text-muted-foreground tabular-nums">
          {t("users.caption", { count: all.length })}
        </p>
      ) : null}

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("users.loadFailed")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("users.loadFailedBody")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("users.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {noMatch ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("users.noMatch", { query: search.trim() })}
        </p>
      ) : null}

      {!failed && !noMatch ? (
        // The wrapper scrolls sideways on a phone (D-64) and, past the page
        // height, down, so the sticky header has something to stick to.
        <div className="max-h-[calc(100dvh-18rem)] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
          <table
            data-slot="table"
            aria-busy={loading || undefined}
            aria-label={t("users.tableLabel")}
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
                : rows.map((u) => (
                    <TableRow key={u.id} className={ROW}>
                      {columns.map((c) => (
                        <TableCell key={c.id} className={cn(CELL, c.className)}>
                          {c.cell(u)}
                        </TableCell>
                      ))}
                    </TableRow>
                  ))}
            </TableBody>
          </table>
        </div>
      ) : null}

      <CreateUserDialog open={createOpen} onOpenChange={setCreateOpen} />
      <DisableUserDialog
        user={disableFor}
        onClose={() => setDisableFor(null)}
        onDone={() => {
          setDisableFor(null);
          refresh();
        }}
        onStale={refresh}
      />
      <DeleteUserDialog
        user={deleteFor}
        onClose={() => setDeleteFor(null)}
        onDone={() => {
          setDeleteFor(null);
          refresh();
        }}
        onStale={refresh}
      />
      <RegenerateInviteDialog
        user={regenerateFor}
        onClose={() => setRegenerateFor(null)}
        onGenerated={(user, link) => {
          setRegenerateFor(null);
          setIssued({ username: user.username, link });
          // An expired invite is pending again.
          refresh();
        }}
        onStale={refresh}
      />
      <OneTimeLinkDialog
        title="invite"
        username={issued?.username ?? ""}
        link={issued?.link ?? null}
        onClose={() => setIssued(null)}
      />
    </div>
  );
}
