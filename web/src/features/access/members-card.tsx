// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

import type { TeamDetail, TeamMember } from "@/api/teams";
import type { UserStatus } from "@/api/users";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { AddMembersPopover } from "@/features/access/add-members-popover";
import { InstanceAdminBadge } from "@/features/access/instance-admin-badge";
import { UserStatusBadge } from "@/features/access/user-status-badge";
import { SettingsCard } from "@/features/environments/settings-card";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

// A row is 36px, and 44px under 768px for touch (D-64).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

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

// MembersCard is the people in a team (D-25), with Add members. Usernames and
// display names render as text only.
export function MembersCard({ team }: MembersCardProps) {
  const t = useT();
  return (
    <SettingsCard title={t("teams.members.title")} id="members">
      <div>
        <AddMembersPopover team={team} />
      </div>
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
            </TableRow>
          </TableHeader>
          <TableBody>
            {(team.members ?? []).map((m) => (
              <TableRow key={m.userId} className={ROW}>
                <TableCell className={CELL}>
                  <MemberName member={m} />
                </TableCell>
                <TableCell className={CELL}>
                  <UserStatusBadge status={m.status as UserStatus} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </table>
      </div>
    </SettingsCard>
  );
}
