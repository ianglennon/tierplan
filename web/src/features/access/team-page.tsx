// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import { teamQuery } from "@/api/teams";
import { NoPermission } from "@/components/full-page-state";
import { Skeleton } from "@/components/ui/skeleton";
import { MembersCard } from "@/features/access/members-card";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Breadcrumb({ name }: { name: string }) {
  const t = useT();
  return (
    <nav aria-label={t("teams.page.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link to="/settings" className="rounded-sm text-accent-ink hover:underline">
            {t("teams.page.breadcrumb.settings")}
          </Link>
        </li>
        <li>
          <Link to="/settings/access" className="rounded-sm text-accent-ink hover:underline">
            {t("teams.page.breadcrumb.access")}
          </Link>
        </li>
        <li>
          <Link to="/settings/access/teams" className="rounded-sm text-accent-ink hover:underline">
            {t("teams.page.breadcrumb.teams")}
          </Link>
        </li>
        <li className="min-w-0">
          <span aria-current="page" className="break-words">
            {name}
          </span>
        </li>
      </ol>
    </nav>
  );
}

// PageSkeleton holds the final layout while the team loads.
function PageSkeleton() {
  const t = useT();
  return (
    <div className="flex flex-col gap-8" aria-busy="true">
      <span role="status" className="sr-only">
        {t("teams.page.loading")}
      </span>
      <div className="flex flex-col gap-2" aria-hidden="true">
        <Skeleton className="h-3 w-64" />
        <Skeleton className="h-8 w-48" />
      </div>
      <Skeleton className="h-32 w-full" aria-hidden="true" />
    </div>
  );
}

interface TeamPageProps {
  teamId: string;
}

// TeamPage is Settings > Access > Teams > {name}: the breadcrumb, the header and
// the Members card. The page is for an instance admin, who holds teams.manage;
// the server answers 403 again.
export function TeamPage({ teamId }: TeamPageProps) {
  const t = useT();
  const allowed = usePermission("teams.manage");
  const query = useQuery({ ...teamQuery(teamId), enabled: allowed });
  const team = query.data;
  useDocumentTitle(team?.name);

  if (!allowed) return <NoPermission />;
  if (!team) return <PageSkeleton />;

  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-2">
        <Breadcrumb name={team.name} />
        <div className="flex min-w-0 flex-col gap-1">
          <h2 className="min-w-0 text-2xl font-semibold break-words">{team.name}</h2>
          <p className="text-sm text-muted-foreground tabular-nums">
            {t("teams.page.members", { count: team.memberCount })}
          </p>
        </div>
      </header>
      <MembersCard team={team} />
    </div>
  );
}
