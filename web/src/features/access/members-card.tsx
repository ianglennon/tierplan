// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { EllipsisVertical } from "lucide-react";
import { useState } from "react";

import { type TeamDetail, type TeamMember, teamsQuery } from "@/api/teams";
import { type UserStatus, usersQuery } from "@/api/users";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { AddMembersPopover } from "@/features/access/add-members-popover";
import { InstanceAdminBadge } from "@/features/access/instance-admin-badge";
import {
  type MembershipToRemove,
  RemoveMemberDialog,
} from "@/features/access/remove-member-dialog";
import { UserStatusBadge } from "@/features/access/user-status-badge";
import { SettingsCard } from "@/features/environments/settings-card";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

// A row is 36px, and 44px under 768px for touch (D-64).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// The search appears once there are more members than this (UI-SPEC).
const SEARCH_ABOVE = 10;

interface MembersCardProps {
  team: TeamDetail;
}

function MemberName({ member }: { member: TeamMember }) {
  return (
    <div className="flex min-w-0 flex-col">
      <div className="flex min-w-0 items-center gap-2">
        <Link
          to="/settings/access/users/$userId"
          params={{ userId: member.userId }}
          className="min-w-0 rounded-sm text-accent-ink hover:underline"
        >
          <TruncatedText className="font-semibold">{member.username}</TruncatedText>
        </Link>
        {member.instanceAdmin ? <InstanceAdminBadge /> : null}
      </div>
      {member.displayName ? (
        <TruncatedText className="text-xs text-muted-foreground">
          {member.displayName}
        </TruncatedText>
      ) : null}
    </div>
  );
}

// matches says whether a member answers the search: a case-insensitive substring
// of the username or display name.
function matches(member: TeamMember, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  return [member.username, member.displayName].some((v) => v.toLowerCase().includes(q));
}

// MembersCard is the people in a team (D-25), with Add members and, on each row,
// Remove from team… (which shows who loses what before it does anything). Past
// ten members it gets a search field, and the table scrolls inside its own box.
// Usernames and display names render as text only.
export function MembersCard({ team }: MembersCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [removing, setRemoving] = useState<MembershipToRemove | null>(null);
  const members = team.members ?? [];
  const rows = members.filter((m) => matches(m, search));
  const noMatch = members.length > 0 && rows.length === 0;

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: teamsQuery.queryKey });
    void queryClient.invalidateQueries({ queryKey: usersQuery.queryKey });
  };

  return (
    <SettingsCard title={t("teams.members.title")} id="members">
      <div className="flex flex-col gap-3 md:flex-row md:items-end md:justify-between">
        {members.length > SEARCH_ABOVE ? (
          <div className="flex flex-col gap-1.5 md:w-80">
            <Label htmlFor="team-members-search" className="text-sm text-muted-foreground">
              {t("teams.members.search.label")}
            </Label>
            <Input
              id="team-members-search"
              type="search"
              value={search}
              placeholder={t("teams.members.search.placeholder")}
              autoComplete="off"
              className="h-11 md:h-8"
              onChange={(e) => setSearch(e.target.value)}
            />
          </div>
        ) : (
          <span />
        )}
        <AddMembersPopover team={team} />
      </div>

      {members.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("teams.members.empty")}</p>
      ) : null}

      {noMatch ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("teams.members.noMatch", { query: search.trim() })}
        </p>
      ) : null}

      {members.length > 0 && !noMatch ? (
        <div className="max-h-96 overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
          <table
            data-slot="table"
            aria-label={t("teams.members.tableLabel", { team: team.name })}
            className="w-full caption-bottom text-sm"
          >
            <TableHeader>
              <TableRow className={`${ROW} hover:bg-transparent`}>
                <TableHead className="sticky top-0 z-10 h-12 bg-muted px-3 text-xs font-semibold text-muted-foreground">
                  {t("teams.members.columns.user")}
                </TableHead>
                <TableHead className="sticky top-0 z-10 h-12 bg-muted px-3 text-xs font-semibold text-muted-foreground">
                  {t("teams.members.columns.status")}
                </TableHead>
                <TableHead className="sticky top-0 z-10 h-12 w-12 bg-muted px-3 text-right text-xs font-semibold text-muted-foreground">
                  {t("teams.members.columns.actions")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((m) => (
                <TableRow key={m.userId} className={ROW}>
                  <TableCell className={CELL}>
                    <MemberName member={m} />
                  </TableCell>
                  <TableCell className={CELL}>
                    <UserStatusBadge status={m.status as UserStatus} />
                  </TableCell>
                  <TableCell className={`${CELL} w-12 text-right`}>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon-sm"
                          className="max-md:size-11"
                          aria-label={t("teams.members.actionsFor", { username: m.username })}
                        >
                          <EllipsisVertical aria-hidden="true" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem
                          variant="destructive"
                          onSelect={() =>
                            setRemoving({
                              team: { id: team.id, name: team.name },
                              user: { id: m.userId, username: m.username },
                            })
                          }
                        >
                          {t("teams.members.remove")}
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </table>
        </div>
      ) : null}

      <RemoveMemberDialog
        membership={removing}
        onClose={() => setRemoving(null)}
        onDone={() => {
          setRemoving(null);
          refresh();
        }}
        onStale={refresh}
      />
    </SettingsCard>
  );
}
