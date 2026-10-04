// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Server, Waypoints } from "lucide-react";
import { type MouseEvent, useRef, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { NoPermission } from "@/components/full-page-state";
import { LiveChip } from "@/components/live-chip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Spinner } from "@/components/ui/spinner";
import { useHealthLive } from "@/features/environments/health-stream";
import { InstallDialog } from "@/features/environments/install-dialog";
import { LocalHostPanel } from "@/features/environments/local-host-panel";
import { EnvOverviewTable } from "@/features/environments/overview-table";
import { SORT_KEYS, type SortDir, type SortKey } from "@/features/environments/sort";
import {
  hasPermission,
  useAnyEnvironmentPermission,
  useDocumentTitle,
  useMe,
  usePermission,
} from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

interface EnvironmentsSearch {
  sort?: SortKey;
  dir?: SortDir;
}

// The sort lives in the URL so a sorted view is a link (D-32). A value the page
// does not know is dropped, so a stale or hand-edited link shows the default
// problems-first order instead of an error.
function validateSearch(search: Record<string, unknown>): EnvironmentsSearch {
  const sort = SORT_KEYS.find((key) => key === search.sort);
  const dir = search.dir === "asc" || search.dir === "desc" ? search.dir : undefined;
  return { sort, dir };
}

// The overview, ENV-06: every environment's health in one place (D-32). The
// server limits the list and the stream to what the caller may read, so this
// only decides what to show; a caller who may read no environment gets the No
// permission page and no sidebar item.
function EnvironmentsPage() {
  const t = useT();
  useDocumentTitle(t("overview.tabTitle"));
  const navigate = Route.useNavigate();
  const { sort, dir } = Route.useSearch();
  const { data: me } = useMe();
  const allowed = useAnyEnvironmentPermission("environments.read");
  const canCreate = usePermission("environments.create");
  const environments = useQuery(environmentsQuery);
  const live = useHealthLive();
  // One install dialog serves "Add environment" and "Finish setup" (D-15).
  const [install, setInstall] = useState<{ open: boolean; mode: "add" | "rotate"; envId?: string }>(
    {
      open: false,
      mode: "add",
    },
  );

  const trigger = useRef<HTMLElement | null>(null);

  if (!allowed) return <NoPermission />;

  const items = environments.data?.items ?? [];
  const loaded = environments.data !== undefined;
  const failed = environments.isError && !loaded;
  const empty = loaded && items.length === 0;
  // The Bayplan host manages a swarm that is enrolled through agents and could be folded
  // into it (D-48). Only someone who may do it is told; the swarm environment is the one
  // the fold happens in, and its settings page holds the card.
  const foldSwarm = items.find(
    (e) => e.kind === "agent" && e.foldOffered && hasPermission(me, "environments.manage"),
  );

  // Whatever opened the dialog, so closing it puts focus back there.
  const addEnvironment = (event: MouseEvent<HTMLElement>) => {
    trigger.current = event.currentTarget;
    setInstall({ open: true, mode: "add" });
  };
  const finishSetup = (envId: string, from: HTMLElement) => {
    trigger.current = from;
    setInstall({ open: true, mode: "rotate", envId });
  };

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <h1 className="text-2xl font-semibold">{t("overview.title")}</h1>
        {loaded ? (
          <span className="text-xs text-muted-foreground tabular-nums">
            {t("overview.caption", { count: items.length })}
          </span>
        ) : null}
        {live === "live" || live === "reconnecting" ? <LiveChip state={live} /> : null}
        {/* One primary button per view: the empty state carries its own. */}
        {canCreate && !empty ? (
          <Button
            className="ms-auto max-md:order-last max-md:h-11 max-md:w-full"
            onClick={addEnvironment}
          >
            {t("overview.add")}
          </Button>
        ) : null}
      </header>

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("overview.error.heading")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("overview.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={environments.isFetching}
              onClick={() => void environments.refetch()}
            >
              {environments.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("overview.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {foldSwarm ? (
        <Alert>
          <Waypoints aria-hidden="true" />
          <AlertTitle>{t("overview.fold.title", { swarm: foldSwarm.name })}</AlertTitle>
          <AlertDescription className="flex flex-col gap-1">
            <span>{t("overview.fold.body")}</span>
            <Link
              to="/settings/environments/$envId"
              params={{ envId: foldSwarm.id }}
              hash="fold"
              className="w-fit rounded-sm text-accent-ink underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:inline-flex max-md:min-h-11 max-md:items-center"
            >
              {t("overview.fold.review")}
            </Link>
          </AlertDescription>
        </Alert>
      ) : null}

      {empty ? (
        <div className="mx-auto flex w-full max-w-120 flex-col gap-6">
          <Empty>
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <Server aria-hidden="true" />
              </EmptyMedia>
              <EmptyTitle>{t("env.empty.heading")}</EmptyTitle>
              <EmptyDescription>{t("env.empty.body")}</EmptyDescription>
            </EmptyHeader>
            {canCreate ? (
              <EmptyContent>
                <Button className="max-md:h-11" onClick={addEnvironment}>
                  {t("overview.add")}
                </Button>
              </EmptyContent>
            ) : null}
          </Empty>
          {canCreate ? (
            <Card className="py-6">
              <CardContent className="px-6">
                <LocalHostPanel mode="settings" />
              </CardContent>
            </Card>
          ) : (
            <p className="text-center text-sm text-muted-foreground">
              {t("env.empty.noPermission")}
            </p>
          )}
        </div>
      ) : null}

      {!failed && !empty ? (
        <EnvOverviewTable
          items={items}
          loading={!loaded}
          sort={sort}
          dir={dir}
          onSortChange={(nextSort, nextDir) =>
            // replace: a sorted view is a link, but every click is not a step
            // for the Back button.
            void navigate({ search: { sort: nextSort, dir: nextDir }, replace: true })
          }
          canFinishSetup={() => hasPermission(me, "environments.token")}
          onFinishSetup={finishSetup}
        />
      ) : null}

      <InstallDialog
        open={install.open}
        mode={install.mode}
        envId={install.envId}
        returnFocusRef={trigger}
        onOpenChange={(open) => setInstall((current) => ({ ...current, open }))}
      />
    </div>
  );
}

export const Route = createFileRoute("/_app/environments")({
  validateSearch,
  component: EnvironmentsPage,
});
