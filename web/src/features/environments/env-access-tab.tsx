// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import {
  type EnvAccessPerson,
  type EnvAccessTeam,
  envAccessCandidatesQuery,
  envAccessQuery,
} from "@/api/envaccess";
import { environmentsQuery } from "@/api/environments";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ExtraPermissionChips } from "@/features/access/extra-permission-chips";
import { GrantDialog } from "@/features/access/grant-dialog";
import { RoleBadge } from "@/features/access/role-badge";
import { SourceBadges } from "@/features/access/source-badges";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Six skeleton rows hold the place of a table while it loads (UI-SPEC "loading").
const SKELETON_ROWS = 6;

// Both tables scroll inside their card however many people there are (UI-SPEC
// "overflow: 100 people"), and sideways under 768px.
const SCROLL = "max-h-96 overflow-auto rounded-md ring-1 ring-foreground/10";

function TeamsTable({ name, teams }: { name: string; teams: readonly EnvAccessTeam[] }) {
  const t = useT();
  return (
    <div className={SCROLL}>
      <Table aria-label={t("envaccess.teams.label", { name })}>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow>
            <TableHead>{t("envaccess.teams.columns.team")}</TableHead>
            <TableHead>{t("envaccess.teams.columns.role")}</TableHead>
            <TableHead>{t("envaccess.teams.columns.extras")}</TableHead>
            <TableHead>{t("envaccess.teams.columns.source")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {teams.map((x) => (
            <TableRow key={x.teamId}>
              <TableCell className="align-top">
                <span className="block max-w-64 truncate" title={x.name}>
                  {x.name}
                </span>
              </TableCell>
              <TableCell className="align-top">
                <RoleBadge role={x.role} />
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <ExtraPermissionChips extras={x.extras ?? []} />
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <SourceBadges sources={x.sources ?? []} linkTeams={false} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function PeopleTable({ name, people }: { name: string; people: readonly EnvAccessPerson[] }) {
  const t = useT();
  return (
    <div className={SCROLL}>
      <Table aria-label={t("envaccess.people.label", { name })}>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow>
            <TableHead>{t("envaccess.people.columns.user")}</TableHead>
            <TableHead>{t("envaccess.people.columns.role")}</TableHead>
            <TableHead>{t("envaccess.people.columns.extras")}</TableHead>
            <TableHead>{t("envaccess.people.columns.source")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {people.map((p) => (
            <TableRow key={p.userId}>
              <TableCell className="align-top">
                <span className="block max-w-64 truncate" title={p.username}>
                  {p.username}
                </span>
                {p.displayName ? (
                  <span className="block max-w-64 truncate text-xs text-muted-foreground">
                    {p.displayName}
                  </span>
                ) : null}
              </TableCell>
              <TableCell className="align-top">
                <RoleBadge role={p.role} />
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <ExtraPermissionChips extras={p.extras ?? []} />
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <SourceBadges sources={p.sources ?? []} linkTeams={false} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function TabSkeleton() {
  const t = useT();
  return (
    <div aria-busy="true" className="flex flex-col gap-1.5">
      <span role="status" className="sr-only">
        {t("envaccess.tab.loading")}
      </span>
      {Array.from({ length: SKELETON_ROWS }, (_, i) => (
        // Placeholder rows have no identity of their own.
        // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
        <Skeleton key={i} className="h-9 w-full" aria-hidden="true" />
      ))}
    </div>
  );
}

interface EnvAccessTabProps {
  envId: string;
}

// EnvAccessTab is the Access tab of an environment's settings page (D-25, D-06):
// everyone who can use the environment and where their access comes from, in two
// tables, Teams and People, with the highest role, the union of the extra
// permissions and every source. "Add access" opens the GrantDialog on this
// environment. The tab is for someone who holds environments.access here, an env
// Admin or an instance admin; the server answers 403 (or 404) otherwise.
export function EnvAccessTab({ envId }: EnvAccessTabProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const environments = useQuery(environmentsQuery);
  const name = environments.data?.items?.find((e) => e.id === envId)?.name ?? "";
  const access = useQuery(envAccessQuery(envId));
  const [adding, setAdding] = useState(false);

  // A saved grant changes the tables and who the Who list offers.
  function refresh() {
    void queryClient.invalidateQueries({ queryKey: envAccessQuery(envId).queryKey });
    void queryClient.invalidateQueries({ queryKey: envAccessCandidatesQuery(envId).queryKey });
  }

  const teams = access.data?.teams ?? [];
  const people = access.data?.people ?? [];
  return (
    <SettingsCard title={t("envaccess.tab.title", { name })}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="min-w-0 text-sm text-muted-foreground break-words">
          {t("envaccess.tab.caption")}
        </p>
        <Button
          type="button"
          className={`${TOUCH_HEIGHT} max-md:w-full`}
          onClick={() => setAdding(true)}
        >
          {t("envaccess.tab.add")}
        </Button>
      </div>
      {access.isPending ? <TabSkeleton /> : null}
      {access.isError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>
              {t("envaccess.tab.loadFailed")} {t("envaccess.tab.loadFailedBody")}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={access.isFetching}
              onClick={() => void access.refetch()}
            >
              {access.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("envaccess.tab.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {access.data ? (
        <>
          {teams.length > 0 ? (
            <section className="flex flex-col gap-2">
              <h4 className="text-sm font-semibold">{t("envaccess.teams.heading")}</h4>
              <TeamsTable name={name} teams={teams} />
            </section>
          ) : null}
          <section className="flex flex-col gap-2">
            <h4 className="text-sm font-semibold">{t("envaccess.people.heading")}</h4>
            <PeopleTable name={name} people={people} />
          </section>
        </>
      ) : null}
      <GrantDialog
        mode="env"
        environment={{ id: envId, name }}
        open={adding}
        onOpenChange={setAdding}
        onSaved={refresh}
      />
    </SettingsCard>
  );
}
