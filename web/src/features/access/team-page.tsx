// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useRouter } from "@tanstack/react-router";
import { useState } from "react";

import { type TeamDetail, TeamNotFoundError, teamQuery, teamsQuery } from "@/api/teams";
import { NoPermission } from "@/components/full-page-state";
import { NotFound } from "@/components/not-found";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { AccessCard } from "@/features/access/access-card";
import { DeleteTeamDialog } from "@/features/access/delete-team-dialog";
import { MembersCard } from "@/features/access/members-card";
import { RenameTeamForm } from "@/features/access/rename-team-form";
import { SettingsCard } from "@/features/environments/settings-card";
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
// the cards, 32px apart: General (rename), Members, Access and Delete team. The page is
// for an instance admin, who holds teams.manage; the server answers 403 again.
export function TeamPage({ teamId }: TeamPageProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const router = useRouter();
  const allowed = usePermission("teams.manage");
  const query = useQuery({ ...teamQuery(teamId), enabled: allowed });
  const team = query.data;
  const [deleting, setDeleting] = useState(false);
  useDocumentTitle(team?.name);

  if (!allowed) return <NoPermission />;
  if (query.error instanceof TeamNotFoundError) return <NotFound />;
  if (query.isError && !team) {
    return (
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
    );
  }
  if (!team) return <PageSkeleton />;

  const refresh = () => void queryClient.invalidateQueries({ queryKey: teamsQuery.queryKey });

  // The name just saved is shown at once, with the version the next save must
  // carry, and the team is read again.
  function renamed(saved: { name: string; updatedAt: string }) {
    queryClient.setQueryData(teamQuery(teamId).queryKey, (old: TeamDetail | undefined) =>
      old ? { ...old, name: saved.name, updatedAt: saved.updatedAt } : old,
    );
    refresh();
  }

  // The team is gone: forget the page and go back to the list it came from.
  function deleted() {
    setDeleting(false);
    queryClient.removeQueries({ queryKey: teamQuery(teamId).queryKey });
    refresh();
    void router.navigate({ to: "/settings/access/teams" });
  }

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
      <SettingsCard title={t("teams.general.title")} id="general">
        <RenameTeamForm
          // A new version of the team starts the form again from its name.
          key={`${team.name}:${team.updatedAt}`}
          team={team}
          onSaved={renamed}
          onStale={refresh}
        />
      </SettingsCard>
      <MembersCard team={team} />
      <AccessCard subject={{ type: "team", id: team.id, name: team.name }} />
      <SettingsCard title={t("teams.danger.title")} id="delete">
        <p className="text-sm text-muted-foreground">{t("teams.danger.body")}</p>
        <div>
          <Button
            type="button"
            variant="outline"
            className="h-11 border-destructive/40 text-destructive hover:text-destructive max-md:w-full md:h-8"
            onClick={() => setDeleting(true)}
          >
            {t("teams.danger.button")}
          </Button>
        </div>
      </SettingsCard>
      <DeleteTeamDialog
        team={deleting ? team : null}
        onClose={() => setDeleting(false)}
        onDone={deleted}
        onStale={refresh}
      />
    </div>
  );
}
